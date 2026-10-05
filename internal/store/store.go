package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // SQLite 驱动（纯 Go 实现，避免 CGO 依赖）

	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// driverSQLite 是当前唯一支持的存储驱动名。
// 其他驱动（如 Postgres）在实现前不开放配置（架构原则 7），Open 直接报错。
const driverSQLite = "sqlite"

// ErrNotFound 表示查询的记录不存在，调用方应以 errors.Is 判断。
var ErrNotFound = errors.New("记录不存在")

// ErrRevisionConflict 表示调用方提交的 revision 已过期，应重新读取后再修改。
var ErrRevisionConflict = errors.New("数据已被其他操作更新")

// ErrProjectInactive 表示修改目标不是当前活动 Project。
var ErrProjectInactive = errors.New("Project 当前未激活")

// ErrProjectArchived 表示 Project 已归档，不能继续写入。
var ErrProjectArchived = errors.New("Project 已归档")

// ErrConversationArchived 表示 Conversation 只允许历史复盘，不能再产生
// 新消息、Run 或会话级配置。归档记录仍然可以通过只读接口查询。
var ErrConversationArchived = errors.New("Conversation 已归档")

// ErrConversationHasActiveWork 表示 Conversation 还有未结束 Run 或待应答
// Interaction。用户必须先显式停止或应答，不能用归档隐藏仍在进行的工作。
var ErrConversationHasActiveWork = errors.New("Conversation 仍有未结束 Run 或待应答 Interaction")

// ErrProjectHasActiveWork 表示 Project 还有未结束 Run 或待应答 Interaction。
// 这些记录必须先通过取消或应答收敛，不能靠归档隐藏正在进行的工作。
var ErrProjectHasActiveWork = errors.New("Project 仍有未结束 Run 或待应答 Interaction")

// ErrDefaultProject 表示请求尝试归档系统为用户保留的 Default Project。
var ErrDefaultProject = errors.New("Default Project 不能归档")

// ErrInvalidState 表示状态值或状态迁移不属于当前实现允许的范围。
var ErrInvalidState = errors.New("状态或状态迁移非法")

// ErrOperatorConfirmationRequired 表示停止请求无法凭 Robot Execution 证据收敛：
// 物理执行状态未知，普通 stop 会被安全策略退回 paused/execution_state_unknown。
// 调用方必须引导用户走人工安全确认入口，不能把停止被回退当成成功。
var ErrOperatorConfirmationRequired = errors.New("执行物理状态未知，必须人工确认现场安全")

// ErrRobotReserved 表示非终态 Task、直接 Run 或物理执行已占用同一 Robot。它不是
// 新的调度状态；Workflow 将其收敛为既有 waiting_resource 后等待设备事件。
var ErrRobotReserved = errors.New("Robot 已被其他工作占用")

// ErrEventGap 表示 Studio 请求的事件位置早于当前可恢复范围。
var ErrEventGap = errors.New("事件增量存在缺口")

// ErrSchemaIncompatible 表示数据库来自已经废弃的开发 schema。调用方应
// 提示用户通过 semantic init --reset-data 备份并重新初始化，不能自动改写。
var ErrSchemaIncompatible = errors.New("STORE_SCHEMA_INCOMPATIBLE")

// Store 是元数据存储的具体类型，持有 SQLite 连接句柄。
// 所有方法通过 database/sql 操作，连接池并发安全。
type Store struct {
	// db 底层数据库连接池。
	db *sql.DB

	// logger 结构化日志器，用于迁移等关键路径记录。
	logger *log.Logger

	// artifactDir 产物内容本体的存放目录（由 SQLite 路径推导，见 Open）。
	artifactDir string

	// databasePath 是清理后的 SQLite 文件路径，用于派生 Project 工作区。
	databasePath string

	// projectWriteMu 把“校验 Conversation 可写并写入首个运行事实”与
	// Project 激活/归档、Conversation 归档串行化。v0.2 是单 Server，
	// 进程内门闩配合 SQLite 单写事务即可关闭检查后状态被切换的窗口。
	projectWriteMu sync.Mutex
}

// Open 按配置打开存储并校验连通性，失败时返回错误。
// 连接池限制为单连接（MaxOpenConns(1)）：SQLite 是单写者模型，
// 多连接并发写只会换来 SQLITE_BUSY 重试，串行化反而更快更稳。
// 注意：Open 不执行迁移，迁移由调用方显式触发（见 Migrate），
// 使"打开"与"变更 schema"两个动作在启动序列中各自可见、可记录。
func Open(cfg config.StoreConfig, logger *log.Logger) (*Store, error) {
	if cfg.Driver != driverSQLite {
		return nil, fmt.Errorf("不支持的存储驱动 %q：首版仅支持 %q", cfg.Driver, driverSQLite)
	}

	db, err := sql.Open(driverSQLite, cfg.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite %s 失败: %w", cfg.SQLitePath, err)
	}
	if err := configureSQLite(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("配置 SQLite %s 失败: %w", cfg.SQLitePath, err)
	}
	// SQLite 中包含前端录入的模型服务 Token。当前版本接受本地明文存储，
	// 但数据库文件必须仅允许当前用户读写。
	if err := os.Chmod(cfg.SQLitePath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("设置 SQLite 文件权限失败: %w", err)
	}

	// 产物目录取 SQLite 库的同级 artifacts 子目录：默认库路径
	// .output/semantic.db → .output/artifacts（与架构约定一致），
	// 且随库路径走，测试用临时库时产物也落在临时目录。
	artifactDir := filepath.Join(filepath.Dir(cfg.SQLitePath), "artifacts")

	logger.Info("元数据存储已连接", "driver", cfg.Driver, "path", cfg.SQLitePath)
	return &Store{db: db, logger: logger, artifactDir: artifactDir,
		databasePath: filepath.Clean(cfg.SQLitePath)}, nil
}

// configureSQLite 为同一份数据库中的业务事务和流式事件提供稳定的并发边界。
// 模型输出时会连续写入 message/reasoning 事件，而调试工具、备份程序或只读
// 查询可能同时持有读事务。WAL 允许这类读事务与单写者并行；busy_timeout 只
// 吸收极短的锁切换窗口，避免一次瞬时竞争把底层连接留在未完成事务中。这里
// 不增加应用层重试：调用方仍会看到持续锁冲突，真正的写入错误不会被掩盖。
func configureSQLite(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return fmt.Errorf("启用 WAL 失败: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("启用 WAL 失败: SQLite 返回 journal_mode=%q", journalMode)
	}
	return nil
}

// Close 关闭存储并释放底层连接池。
func (s *Store) Close() error {
	return s.db.Close()
}
