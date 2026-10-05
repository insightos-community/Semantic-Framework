package pilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Complete joint trajectories are carried in a single JSON-RPC line.
const maxWorkerMessageBytes = 8 * 1024 * 1024

type rpcMessage struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   map[string]any `json:"error,omitempty"`

	// 这两个序号只在 Pilot 进程内使用，不属于 JSON-RPC 协议。Worker 的 stdout
	// 是有序的，但事件和调用结果会被不同 goroutine 消费；用读入顺序建立屏障，
	// 可以保证 skill.run 终态不会越过已经读到的最后一个 Stage 事件。
	requestSequence uint64
	requestBarrier  uint64
}

type WorkerSupervisor struct {
	Installer        SkillInstaller
	PythonExecutable string
	PythonPaths      []string
	OnStderrLine     func(string)
}

type WorkerProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	// Captured once from the installed Python input_model before Start returns.
	// This is not synthesized from documentation or examples.
	inputSchema map[string]any

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan rpcMessage
	requests  chan rpcMessage
	done      chan struct{}
	waitErrMu sync.RWMutex
	waitErr   error
	nextID    atomic.Uint64
	serveOnce sync.Once

	requestQueued   atomic.Uint64
	requestHandled  atomic.Uint64
	requestProgress chan struct{}
}

func (s WorkerSupervisor) Start(ctx context.Context, definition SkillDefinition) (*WorkerProcess, error) {
	python := s.PythonExecutable
	paths := append([]string{}, s.PythonPaths...)
	if s.Installer != nil {
		prepared, err := s.Installer.Prepare(ctx, definition)
		if err != nil {
			return nil, err
		}
		python = prepared.PythonExecutable
		paths = append(paths, prepared.PythonPaths...)
	}
	if python == "" {
		python = "python"
	}
	command := exec.CommandContext(ctx, python, "-m", "semantic_robot_skill_sdk.worker", "--skill-dir", definition.Directory)
	paths = append(paths, filepath.Dir(filepath.Dir(definition.Directory)))
	command.Env = append(os.Environ(), "PYTHONPATH="+strings.Join(paths, string(os.PathListSeparator)))
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &WorkerProcess{
		command:         command,
		stdin:           stdin,
		pending:         make(map[string]chan rpcMessage),
		requests:        make(chan rpcMessage, 32),
		done:            make(chan struct{}),
		requestProgress: make(chan struct{}, 1),
	}
	go process.readStdout(stdout)
	go process.readStderr(stderr, s.OnStderrLine)
	go func() {
		err := command.Wait()
		process.waitErrMu.Lock()
		if process.waitErr == nil {
			process.waitErr = err
		}
		process.waitErrMu.Unlock()
		close(process.done)
	}()

	select {
	case message := <-process.requests:
		if message.Method != "ready" {
			_ = process.Kill()
			return nil, fmt.Errorf("worker first message is %q, expected ready", message.Method)
		}
		process.markRequestHandled(message.requestSequence)
		initialized, initErr := process.Call(ctx, "worker.initialize", map[string]any{
			"name":    definition.Name,
			"version": definition.Version,
			"runtime": map[string]any{
				"api_version":     definition.Runtime.APIVersion,
				"entrypoint":      definition.Runtime.Entrypoint,
				"stop_entrypoint": definition.Runtime.StopEntrypoint,
				"input_model":     definition.Runtime.InputModel,
				"state_model":     definition.Runtime.StateModel,
				"result_model":    definition.Runtime.ResultModel,
				"controllers":     definition.Runtime.Controllers,
			},
		})
		if initErr != nil {
			_ = process.Kill()
			return nil, fmt.Errorf("initialize worker: %w", initErr)
		}
		if result, ok := initialized.(map[string]any); ok {
			process.inputSchema, _ = result["input_schema"].(map[string]any)
		}
		return process, nil
	case <-process.done:
		return nil, fmt.Errorf("worker exited before ready: %w", process.exitError())
	case <-ctx.Done():
		_ = process.Kill()
		return nil, ctx.Err()
	}
}

type WorkerHandler func(context.Context, string, map[string]any) (any, error)

// Run 在一个循环中处理 Worker 发来的 Action、Observation、Agent 请求和事件。
// stop 请求可以从其他 goroutine 并发发送，读 stdout 的 goroutine 仍只有一个。
func (p *WorkerProcess) Run(ctx context.Context, params map[string]any, handler WorkerHandler) (map[string]any, error) {
	// 请求分发器属于 Worker 进程，而不是某一次 skill.run。普通执行在停止期间可能先
	// 返回 stopping，但 stop entrypoint 仍需要继续调用 Action 并取得停止证据。
	p.serveOnce.Do(func() { go p.serveRequests(ctx, handler) })
	value, err := p.Call(ctx, "skill.run", params)
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("worker returned non-object result")
	}
	return result, nil
}

func (p *WorkerProcess) serveRequests(ctx context.Context, handler WorkerHandler) {
	for {
		select {
		case message := <-p.requests:
			result, err := handler(ctx, message.Method, message.Params)
			if message.ID != nil {
				if err != nil {
					_ = p.write(rpcMessage{JSONRPC: "2.0", ID: message.ID, Error: map[string]any{"code": -32020, "message": err.Error()}})
				} else {
					_ = p.write(rpcMessage{JSONRPC: "2.0", ID: message.ID, Result: result})
				}
			}
			p.markRequestHandled(message.requestSequence)
		case <-ctx.Done():
			return
		}
	}
}

func (p *WorkerProcess) Stop(ctx context.Context, request map[string]any) (map[string]any, error) {
	value, err := p.Call(ctx, "skill.stop", map[string]any{"request": request})
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("worker stop returned non-object result")
	}
	return result, nil
}

func (p *WorkerProcess) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	id, response := p.startCall(method, params)
	defer p.removePending(id)
	select {
	case message := <-response:
		if message.Error != nil {
			return nil, fmt.Errorf("worker %s failed: %v", method, message.Error["message"])
		}
		if err := p.waitRequestsHandled(ctx, message.requestBarrier); err != nil {
			return nil, err
		}
		return message.Result, nil
	case <-p.done:
		return nil, p.exitError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *WorkerProcess) exitError() error {
	p.waitErrMu.RLock()
	defer p.waitErrMu.RUnlock()
	if p.waitErr == nil {
		return errors.New("worker process exited")
	}
	return p.waitErr
}

func (p *WorkerProcess) Shutdown(ctx context.Context) error {
	_, err := p.Call(ctx, "worker.shutdown", map[string]any{})
	_ = p.stdin.Close()
	return err
}

func (p *WorkerProcess) Kill() error {
	_ = p.stdin.Close()
	if p.command.Process == nil {
		return nil
	}
	killErr := p.command.Process.Kill()
	// Pilot 关闭或 Execution 清理后，上层会立即回收 Skill 工作目录。仅向
	// 子进程发送 kill 就返回会留下一个仍在写 checkpoint 的窗口，导致目录
	// 清理与下一次启动发生竞争。command.Wait 由启动时的固定 goroutine执行，
	// 因此这里等待 done，不会重复 Wait，也不会丢失进程退出状态。
	<-p.done
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return nil
}

func (p *WorkerProcess) startCall(method string, params map[string]any) (string, chan rpcMessage) {
	id := fmt.Sprintf("pilot-%d", p.nextID.Add(1))
	response := make(chan rpcMessage, 1)
	p.pendingMu.Lock()
	p.pending[id] = response
	p.pendingMu.Unlock()
	if err := p.write(rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		response <- rpcMessage{Error: map[string]any{"message": err.Error()}}
	}
	return id, response
}

func (p *WorkerProcess) removePending(id string) {
	p.pendingMu.Lock()
	delete(p.pending, id)
	p.pendingMu.Unlock()
}

func (p *WorkerProcess) write(message rpcMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(encoded, '\n'))
	return err
}

func (p *WorkerProcess) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxWorkerMessageBytes)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || message.JSONRPC != "2.0" {
			protocolErr := fmt.Errorf("worker stdout contains non JSON-RPC data")
			p.recordExitError(protocolErr)
			p.failPending(protocolErr)
			_ = p.Kill()
			return
		}
		if message.Method != "" {
			message.requestSequence = p.requestQueued.Add(1)
			p.requests <- message
			continue
		}
		id := fmt.Sprint(message.ID)
		p.pendingMu.Lock()
		target := p.pending[id]
		p.pendingMu.Unlock()
		if target == nil {
			protocolErr := fmt.Errorf("worker returned unknown response id %s", id)
			p.recordExitError(protocolErr)
			p.failPending(protocolErr)
			_ = p.Kill()
			return
		}
		// readStdout按Worker写出顺序读取。响应之前已经读取的通知必须先完成
		// 持久化，调用方才能把这个响应收敛为execution.terminal。
		message.requestBarrier = p.requestQueued.Load()
		target <- message
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		p.recordExitError(err)
		p.failPending(err)
		return
	}
	// stdout EOF 本身就是 Worker 已经无法继续履行 JSON-RPC 调用的证据。
	// 不能只等待 exec.Cmd.Wait：子进程或其依赖若继承了管道描述符，Wait/管道
	// 回收的先后顺序可能让 skill.run 永久停在 pending。这里直接结束所有尚未
	// 收到响应的调用；没有 pending 调用的正常 shutdown 不受影响。
	err := errors.New("worker stdout closed before JSON-RPC response")
	if p.failPending(err) {
		p.recordExitError(err)
	}
}

func (p *WorkerProcess) markRequestHandled(sequence uint64) {
	if sequence == 0 {
		return
	}
	p.requestHandled.Store(sequence)
	if p.requestProgress != nil {
		select {
		case p.requestProgress <- struct{}{}:
		default:
		}
	}
}

func (p *WorkerProcess) waitRequestsHandled(ctx context.Context, barrier uint64) error {
	if barrier == 0 {
		return nil
	}
	for p.requestHandled.Load() < barrier {
		select {
		case <-p.requestProgress:
		case <-p.done:
			return p.exitError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *WorkerProcess) readStderr(reader io.Reader, sink func(string)) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if sink != nil {
			sink(scanner.Text())
		}
	}
}

func (p *WorkerProcess) failPending(err error) bool {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	hadPending := len(p.pending) != 0
	for _, target := range p.pending {
		select {
		case target <- rpcMessage{Error: map[string]any{"message": err.Error()}}:
		default:
		}
	}
	return hadPending
}

// recordExitError 只保留首个可解释错误。JSON-RPC 解析/路由错误通常发生在
// Kill 之前，如果被后续的 signal killed 覆盖，Pilot 和测试都无法定位根因。
func (p *WorkerProcess) recordExitError(err error) {
	p.waitErrMu.Lock()
	if p.waitErr == nil {
		p.waitErr = err
	}
	p.waitErrMu.Unlock()
}
