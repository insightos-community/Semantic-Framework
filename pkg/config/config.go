package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 semantic-server 的根配置，只建模当前已有消费方的配置段。
type Config struct {
	// Server HTTP 服务配置。
	Server ServerConfig `yaml:"server"`

	// Log 日志配置。
	Log LogConfig `yaml:"log"`

	// Store 元数据存储配置。
	Store StoreConfig `yaml:"store"`

	// LLM 大模型提供方配置。
	LLM LLMConfig `yaml:"llm"`

	// Agents Agent 运行时配置。
	Agents AgentsConfig `yaml:"agents"`

	// Skills 技能系统配置。
	Skills SkillsConfig `yaml:"skills"`

	// Execution 命令执行工具的服务端总开关。
	Execution ExecutionConfig `yaml:"execution"`

	// Simulation 管理 Runtime 安装清单。清单来自管理员维护的本地目录。
	Simulation SimulationConfig `yaml:"simulation"`

	// RobotRuntime 配置 Server 托管的 Robot 类型包与实例数据目录。
	RobotRuntime RobotRuntimeConfig `yaml:"robot_runtime"`

	// MCPServers MCP server 连接清单（docs/architecture/16 §3）。
	MCPServers []MCPServerConfig `yaml:"mcp_servers"`
}

// ExecutionConfig 定义执行工具的服务端硬边界。会话只能在该边界内收紧或
// 临时开启能力，不能通过前端设置突破服务端禁用状态。
type ExecutionConfig struct {
	// AllowHost 是否允许会话显式开启宿主执行；默认关闭。
	AllowHost bool `yaml:"allow_host"`
}

// SimulationConfig 指向 RuntimeInstallation 清单目录。目录中的每个 YAML
// 文件只允许声明固定结构的启动参数，不执行 shell，也不保存 Project 数据。
type SimulationConfig struct {
	RuntimesDir string `yaml:"runtimes_dir"`
	CatalogDir  string `yaml:"catalog_dir"`
}

// RobotRuntimeConfig 只包含 Server 作为 supervisor 所需的部署路径和
// 回连地址。具体 SDK、Ability 与 Tool 配置仍由只读 Bundle 模板决定。
type RobotRuntimeConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BundlesDir string `yaml:"bundles_dir"`
	DataRoot   string `yaml:"data_root"`
	ServerHTTP string `yaml:"server_http_url"`
	ServerWS   string `yaml:"server_websocket_url"`
	PortFirst  int    `yaml:"ability_port_first"`
	PortLast   int    `yaml:"ability_port_last"`

	// ReadinessTimeout 是受管 Robot 启动后等待 AbilityFramework 与 Ability
	// 就绪的上限。零值表示用内置默认值（2 分钟）。冷启动加载大模型的
	// Bundle（如 Franka SmolVLA 走 CPU 推理）可能需要更长，可用本项或
	// 环境变量 SEMANTIC_ROBOT_RUNTIME_READINESS_TIMEOUT 覆盖。
	ReadinessTimeout Duration `yaml:"readiness_timeout"`
}

// SkillsConfig 定义技能系统配置（docs/architecture/06）。
type SkillsConfig struct {
	// Dir 技能根目录（每技能一目录含 SKILL.md，支持分类子目录嵌套）；
	// 目录内文件变更经 store 监听自动热更（500ms 去抖）。
	Dir string `yaml:"dir"`
}

// MCPServerConfig 定义单个 MCP server 的连接参数（pkg/mcp.ServerConfig
// 的配置视图；json tag 供 SEMANTIC_MCP_SERVERS 环境变量覆盖解析）。
type MCPServerConfig struct {
	// Name server 唯一标识。
	Name string `yaml:"name" json:"name"`

	// Transport 传输类型：http（streamable）| stdio。
	Transport string `yaml:"transport" json:"transport"`

	// Endpoint streamable HTTP 的 MCP endpoint URL（http 传输必填）。
	Endpoint string `yaml:"endpoint" json:"endpoint"`

	// Command stdio 子进程可执行文件（stdio 传输必填）。
	Command string `yaml:"command" json:"command"`

	// Args stdio 子进程参数。
	Args []string `yaml:"args" json:"args"`

	// Env stdio 子进程追加的环境变量（"K=V" 形式）。
	Env []string `yaml:"env" json:"env"`

	// Enabled 是否启用（false 的条目保留配置但不连接）。
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Namespace 工具命名空间前缀（预留：R16 目录层以 server 名即命名空间，
	// 见 internal/mcpregistry doc.go 第 6 点；本字段暂未消费）。
	Namespace string `yaml:"namespace" json:"namespace"`

	// Risk 该 server 全部工具的风险等级覆盖（low|medium|high|critical；
	// 空 = medium）。写操作类 server 应显式提级（high 触发 L4 人工审批）。
	Risk string `yaml:"risk" json:"risk"`
}

// AgentsConfig 定义 Agent 运行时配置（docs/architecture/04 §9）。
type AgentsConfig struct {
	// ProfilesDir 角色 profile 根目录（configs/agents）。
	ProfilesDir string `yaml:"profiles_dir"`

	// TeamsDir Team 定义目录（configs/agents/teams）；
	// 目录不存在视为未配置 Team（单 leader 模式）。
	TeamsDir string `yaml:"teams_dir"`
}

// ServerConfig 定义 HTTP 服务的监听与超时配置。
type ServerConfig struct {
	// AccessTokenTTL 用户登录令牌有效期；零值采用认证服务默认值。
	AccessTokenTTL Duration `yaml:"access_token_ttl"`

	// HTTPAddr HTTP 服务监听地址，如 ":8080"。
	HTTPAddr string `yaml:"http_addr"`

	// WSAddr WebSocket 服务监听地址，如 ":8081"。
	WSAddr string `yaml:"ws_addr"`

	// ReadTimeout HTTP 请求读取超时。
	ReadTimeout Duration `yaml:"read_timeout"`

	// WriteTimeout HTTP 响应写入超时。
	WriteTimeout Duration `yaml:"write_timeout"`
}

// LogConfig 定义日志配置。
type LogConfig struct {
	// Level 日志最低输出级别：trace/debug/info/warn/error/fatal。
	Level string `yaml:"level"`
}

// StoreConfig 定义元数据存储配置。
type StoreConfig struct {
	// Driver 存储驱动，当前支持 "sqlite"。
	Driver string `yaml:"driver"`

	// SQLitePath SQLite 数据库文件路径，Driver 为 sqlite 时生效。
	SQLitePath string `yaml:"sqlite_path"`
}

// LLMConfig 定义 LLM 提供方清单与全局默认模型（docs/architecture/02 §5）。
// api_key 不进入配置，只从环境变量 SEMANTIC_LLM_API_KEY_<名称大写> 读取。
type LLMConfig struct {
	// Default 全局默认模型名，必须是 Providers 中的条目名。
	Default string `yaml:"default"`

	// Providers 模型端点清单：名称即唯一标识，使用中立的模型名，不绑定用途。
	Providers map[string]LLMProviderConfig `yaml:"providers"`
}

// LLMProviderConfig 定义单个模型端点。
type LLMProviderConfig struct {
	// Service 模型服务标识（如 openai/deepseek）；同服务端点共享凭据。
	Service string `yaml:"service"`

	// Component 驱动（内核组件）：openai（OpenAI 兼容）|
	// claude（Anthropic Messages API）| mock（无 key 开发测试）。
	Component string `yaml:"component"`

	// BaseURL 模型服务地址；Claude 留空时使用官方 SDK 默认地址。
	BaseURL string `yaml:"base_url"`

	// Model 端点上的模型 ID。
	Model string `yaml:"model"`

	// Capabilities 能力标签：text / image / tool_call / embedding。
	Capabilities []string `yaml:"capabilities"`

	// Options 调用默认参数。白名单见 internal/agent/kernel/factory.go 的
	// applyOptions：temperature / max_tokens / reasoning_effort /
	// timeout_seconds；未识别的键在启动与热重载时报错。
	Options map[string]any `yaml:"options"`

	// Price 每 1K tokens 单价，计量估算用。
	Price LLMPriceConfig `yaml:"price"`
}

// LLMPriceConfig 定义每 1K tokens 的单价。
type LLMPriceConfig struct {
	// Prompt 输入侧每 1K tokens 单价。
	Prompt float64 `yaml:"prompt"`

	// Completion 输出侧每 1K tokens 单价。
	Completion float64 `yaml:"completion"`
}

// Duration 封装 time.Duration，使 yaml 中的 "10s" 风格字符串可被解析。
type Duration time.Duration

// UnmarshalYAML 从 yaml 标量（如 "10s"）解析时长。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("时长字段必须为字符串: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("解析时长 %q 失败: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML 将时长序列化为 "10s" 风格字符串（与 UnmarshalYAML 互逆），
// 供配置树导出（settings 快照序列化与 PATCH 写回）使用。
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

// String 返回时长的可读字符串表示。
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Default 返回与 configs/semantic-server.yaml 一致的默认配置。
// 修改默认值时必须同步修改该 yaml，保证二者是唯一事实源的两个视图。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			AccessTokenTTL: Duration(24 * time.Hour),
			HTTPAddr:       ":8080",
			WSAddr:         ":8081",
			ReadTimeout:    Duration(10 * time.Second),
			WriteTimeout:   Duration(10 * time.Second),
		},
		Log: LogConfig{
			Level: "info",
		},
		Store: StoreConfig{
			Driver:     "sqlite",
			SQLitePath: ".output/semantic.db",
		},
		LLM: LLMConfig{
			Default: "mock",
			Providers: map[string]LLMProviderConfig{
				"mock": {
					Component:    "mock",
					Model:        "mock",
					Capabilities: []string{"text", "tool_call"},
					Price:        LLMPriceConfig{Prompt: 0, Completion: 0},
				},
			},
		},
		Agents: AgentsConfig{
			ProfilesDir: "configs/agents",
			TeamsDir:    "configs/agents/teams",
		},
		Skills: SkillsConfig{
			Dir: "configs/skills",
		},
		Execution: ExecutionConfig{AllowHost: false},
		Simulation: SimulationConfig{
			RuntimesDir: "configs/runtimes.d",
			CatalogDir:  "configs/scenes.d",
		},
		RobotRuntime: RobotRuntimeConfig{
			Enabled: false, BundlesDir: ".output/robot-bundles",
			DataRoot: ".output", ServerHTTP: "http://127.0.0.1:8080",
			ServerWS: "ws://127.0.0.1:8081/ws/pilot", PortFirst: 18100, PortLast: 18199,
		},
		// 显式空列表而非 nil：与 yaml "mcp_servers: []" 及 env JSON "[]"
		// 解析结果一致，热重载 diff（reflect.DeepEqual）不受 nil/空切片
		// 差异干扰。
		MCPServers: []MCPServerConfig{},
	}
}

// ParseYAML 从模板或文件内容加载配置，但不读取进程环境变量。semantic
// init 用它从内置只读模板生成安装配置，避免把当前进程 env 固化进文件。
func ParseYAML(data []byte) (*Config, error) {
	cfg := Default()
	if err := validateYAML(data); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 YAML 失败: %w", err)
	}
	return cfg, nil
}

// Load 加载配置：先填充默认值，再叠加 path 指定的 yaml 文件，
// 最后应用 SEMANTIC_ 前缀的环境变量覆盖。path 为空时跳过文件加载。
// yaml 文件先经 validateYAML 严格校验（未知键/类型错误聚合报错），
// 校验不过直接失败，避免笔误配置被静默忽略后带隐患启动。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		cfg, err = ParseYAML(data)
		if err != nil {
			return nil, fmt.Errorf("校验配置文件 %s 失败: %w", path, err)
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 应用环境变量覆盖。约定：前缀 SEMANTIC_，嵌套字段以 _ 分隔，
// 例如 SEMANTIC_SERVER_HTTP_ADDR 覆盖 server.http_addr。
// 时长类环境变量值非法时直接报错，避免静默使用非预期配置启动服务。
func applyEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("SEMANTIC_SERVER_ACCESS_TOKEN_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("环境变量 SEMANTIC_SERVER_ACCESS_TOKEN_TTL 必须为正时长，例如 720h")
		}
		cfg.Server.AccessTokenTTL = Duration(d)
	}
	if v, ok := os.LookupEnv("SEMANTIC_SERVER_HTTP_ADDR"); ok {
		cfg.Server.HTTPAddr = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_SERVER_WS_ADDR"); ok {
		cfg.Server.WSAddr = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_SERVER_READ_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_SERVER_READ_TIMEOUT 值 %q 非法: %w", v, err)
		}
		cfg.Server.ReadTimeout = Duration(d)
	}
	if v, ok := os.LookupEnv("SEMANTIC_SERVER_WRITE_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_SERVER_WRITE_TIMEOUT 值 %q 非法: %w", v, err)
		}
		cfg.Server.WriteTimeout = Duration(d)
	}
	if v, ok := os.LookupEnv("SEMANTIC_LOG_LEVEL"); ok {
		cfg.Log.Level = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_STORE_DRIVER"); ok {
		cfg.Store.Driver = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_STORE_SQLITE_PATH"); ok {
		cfg.Store.SQLitePath = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_AGENTS_PROFILES_DIR"); ok {
		cfg.Agents.ProfilesDir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_AGENTS_TEAMS_DIR"); ok {
		cfg.Agents.TeamsDir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_SKILLS_DIR"); ok {
		cfg.Skills.Dir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_SIMULATION_RUNTIMES_DIR"); ok {
		cfg.Simulation.RuntimesDir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_SIMULATION_CATALOG_DIR"); ok {
		cfg.Simulation.CatalogDir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_ENABLED"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_ROBOT_RUNTIME_ENABLED 值 %q 非法: %w", v, err)
		}
		cfg.RobotRuntime.Enabled = enabled
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_BUNDLES_DIR"); ok {
		cfg.RobotRuntime.BundlesDir = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_DATA_ROOT"); ok {
		cfg.RobotRuntime.DataRoot = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_SERVER_HTTP"); ok {
		cfg.RobotRuntime.ServerHTTP = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_SERVER_WS"); ok {
		cfg.RobotRuntime.ServerWS = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_PORT_FIRST"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_ROBOT_RUNTIME_PORT_FIRST 值 %q 非法: %w", v, err)
		}
		cfg.RobotRuntime.PortFirst = port
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_PORT_LAST"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_ROBOT_RUNTIME_PORT_LAST 值 %q 非法: %w", v, err)
		}
		cfg.RobotRuntime.PortLast = port
	}
	if v, ok := os.LookupEnv("SEMANTIC_ROBOT_RUNTIME_READINESS_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_ROBOT_RUNTIME_READINESS_TIMEOUT 值 %q 非法: %w", v, err)
		}
		cfg.RobotRuntime.ReadinessTimeout = Duration(d)
	}
	if v, ok := os.LookupEnv("SEMANTIC_LLM_DEFAULT"); ok {
		cfg.LLM.Default = v
	}
	if v, ok := os.LookupEnv("SEMANTIC_MCP_SERVERS"); ok {
		// JSON 数组整体替换（列表类配置无法逐键覆盖）；
		// 非法 JSON 直接报错，避免静默吞掉整段配置。
		var servers []MCPServerConfig
		if err := json.Unmarshal([]byte(v), &servers); err != nil {
			return fmt.Errorf("环境变量 SEMANTIC_MCP_SERVERS 不是合法 JSON 数组: %w", err)
		}
		cfg.MCPServers = servers
	}
	return nil
}
