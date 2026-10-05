package skill

// Skill 是一个已加载的技能：SKILL.md 的解析结果（架构文档 06 §4）。
// 标准字段（Name/Description/Category/WhenToUse）驱动渐进披露注入；
// 具身扩展字段经 Extensions 原样透传——本轮只解析不消费（Phase 3 由
// 任务/安全模块消费，故不建对应的空结构体）。
type Skill struct {
	// Name 技能名（frontmatter name，必填）：全局唯一标识，同名后加载覆盖先加载。
	Name string

	// Description 一句话描述（frontmatter description，必填）：
	// 渐进披露的清单摘要与模型的匹配判断依据。
	Description string

	// Category 技能分类（frontmatter category，空时回填 DefaultCategory）：
	// Summary 按分类分组渲染。
	Category string

	// WhenToUse 适用时机描述（frontmatter when_to_use，可选）：
	// 本轮只解析透传（匹配信号落地后消费）。
	WhenToUse string

	// Body frontmatter 之后的 markdown 正文：技能命中后 inline 注入的执行指导。
	Body string

	// Dir SKILL.md 所在目录：技能配套资源（scripts/references）的定位基准。
	Dir string

	// Extensions 具身扩展字段的透传集（frontmatter 中标准字段以外的键，
	// 如 goal/safety_rules/component/steps/on_failure）；无扩展字段时为 nil。
	// 只读约定：Store 快照共享本 map，消费方不得改写。
	Extensions map[string]any
}
