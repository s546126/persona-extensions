# Study 0.3.0 设计规格（Learn Your Way：个性化理解 + 双重编码）

> 状态：设计定稿（基于 `study 0.2.0` / `feat/study-tutor` `@ e6fa68bb`），供后续实现分支 `feat/study-learn-your-way` 按模块落地。

---

## 1. 背景与定位

`study 0.2.0` 已经跑通「记牢与纠错」的后半程闭环：多源导入（知识库主题 / Notion / Obsidian / Logseq / NotebookLM / 对话）→ FSRS 间隔重复调度 → `mastery.go`（0–100 知识点掌握度评分与 `weak` / `shaky` / `solid` / `new` 分级）→ `tutor.go`（4 种练习题型、3 级苏格拉底提示、6 类错因诊断与强化卡草稿）。

Google 的 **Learn Your Way** 补充了前半程「第一次就学懂」的关键两环：
1. **千人千面重构**：先采集学习者的背景深度与兴趣领域，在不删减核心知识点的前提下调整讲解深度，并将抽象案例替换成学习者熟悉领域（如系统架构、游戏、体育、烹饪）的类比。
2. **双重编码的多形态学习**：用同一份材料生成可互动的多模态视图（分步图文精读 + 随堂微测、按结构分层的思维导图、弱项音频复盘等），提高首次理解与 3–5 天后的记忆留存。

**0.3.0 设计原则：**
- **补前半程，守住后半程**：在 `study` 前置「精读（Guide）+ 知识导图（Map）+ 学习者画像（Profile）」，下游仍全部汇入现有的待确认草稿 → FSRS 队列 → 深度辅导闭环，不另造第二套记忆系统。
- **纯 Go + 零新增网络权限**：完全复用 hub `models` 与 `persona-view` 原生组件，不新增外部网络请求域名，不塞重型 PDF 解析或重型二进制（复杂扫描 PDF 仍交外部工具转 Markdown 后导入）。
- **移动端优先与向后兼容**：所有页面严格适配 iPhone `persona-view`（通过 `wire.CheckView` 与 `CheckViewJSON`），旧版 `stateVersion=1` JSON 无痛读取，未填写学习者画像时行为与 `0.2.0` 完全一致。

---

## 2. 版本范围拆分

| 版本 | 范围 | 说明 |
| --- | --- | --- |
| **0.3.0（本规格重点落地，P0）** | **M1 学习者画像 + M2 分步互动精读（`Guide`）+ M3 掌握度着色知识导图（`Map`）+ MCP 工具 / Skill / 清单同步** | 全部在现有 Go 扩展内闭环，无需新增权限或外部二进制。 |
| **0.3.x / 0.4.0 预留（P1，本次不实现）** | 薄弱点双人课堂微播客、高频错题按需 SVG 配图 | 播客优先复用已接入的 `notebooklm_command` Audio Overview 或外部 TTS；SVG 配图仅对反复 `concept` 错因且高 `lapses` 的卡片按需生成。 |

---

## 3. M1 · 学习者画像（Learner Profile）

### 3.1 配置项与校验（`app.go` 与 `persona-extension.json`）

在 `app.go` 的 `config` 结构体新增 3 个可选字段（JSON `omitempty` 或零值兼容）：

- `LearnerLevel string`（json: `learner_level`）：学习深度偏好
  - 允许值：`""`（默认，均衡）、`"beginner"`（入门直觉优先）、`"practical"`（工程/实战案例优先）、`"deep"`（原理与源码深读）
- `LearnerInterests string`（json: `learner_interests`）：个人兴趣与类比池
  - 纯文本短描述，例如 `"分布式系统、篮球、做饭"`，经字符裁剪限制 ≤ 200 字符
- `ExplainStyle string`（json: `explain_style`）：讲解风格
  - 允许值：`""` / `"balanced"`（标准）、`"analogy"`（先打比方再下定义）、`"rigorous"`（严密定义与边界条件优先）

在 `(c *config) normalize()` 中对未知枚举回退为 `""`（或 `"balanced"`），对 `LearnerInterests` 做首尾去空、折叠换行与字符长度截断（≤ 200）。
同步在 `persona-extension.json` 的 `configuration.properties` 中登记上述 3 项，供手机端「设置 › 扩展 › 学习」编辑。

### 3.2 画像注入与提示词安全（新增 `profile.go`）

新增 `profile.go`，提供统一的画像片段构造器 `formatLearnerProfile(c config) string`：
1. 若三项均为空，返回 `""`（不额外占用 token，保持 `0.2.0` 提示词不变）。
2. 先做 **prompt-injection 清洗**：剥离 `<learner_profile>`、`</learner_profile>`、`<material>`、`</material>`、`<exercise>`、`</exercise>`、`<answer>`、`</answer>` 等所有大小写变体的 XML 围栏标签及控制字符，避免配置项自身破坏提示词边界。
3. 以 `<learner_profile>` 数据块传入以下 prompt 模板：
   - 新增的 `prompts/guide.md`（精读章节生成与感兴趣领域类比）
   - 现有的 `prompts/cards.md`、`prompts/exercise.md`（尤其增强 `scenario` 场景变式题的贴合度）、`prompts/judge.md`、`prompts/reinforce.md`
4. **两条硬约束写入提示词**：
   - 不得删减、篡改原材料的核心概念、事实或技术约束；
   - 不得因为使用兴趣类比而引入材料之外的虚构结论或替代标准术语。

---

## 4. M2 · 先精读、再刷卡（`Guide` 模块 + 互动滑卡）

### 4.1 持久化数据结构（`store.go`）

保持 `stateVersion = 1`，在 `state` 中新增可选字段 `Guides map[string]*Guide`（`json:"guides,omitempty"`）：

```go
type MicroCheck struct {
	Question string   `json:"question"`
	Answer   string   `json:"answer"`
	Accepted []string `json:"accepted,omitempty"`
}

type GuideSection struct {
	Title      string     `json:"title"`                 // 本节小标题，≤ 80 字
	Intuition  string     `json:"intuition"`             // 一句话核心直觉，≤ 300 字
	Analogy    string     `json:"analogy,omitempty"`     // 结合兴趣的类比（可空），≤ 400 字
	KeyPoints  []string   `json:"key_points"`            // 关键要点（1–5 条），每条 ≤ 240 字
	MicroCheck MicroCheck `json:"micro_check,omitempty"` // 随堂微测（可选）
}

type Guide struct {
	ID       string         `json:"id"`
	Topic    string         `json:"topic"`
	Title    string         `json:"title"`
	Source   Source         `json:"source"`
	Sections []GuideSection `json:"sections"` // 1–6 节
	Plain    bool           `json:"plain,omitempty"`
	Created  time.Time      `json:"created"`
}
```

- 新增 `const maxGuides = 100`；超出时按 `Created` 淘汰最旧的 Guide（同步清理并限制每条字段长度，防损坏 JSON 导致越界或崩溃）。

### 4.2 精读生成与离线降级（新增 `guide.go` + `prompts/guide.md`）

支持两种入口生成精读：
1. **按知识点（`topic`）生成**：聚合该 `topic` 下现有卡片（优先排入薄弱卡片 `statusWeak` / `statusShaky`）以及近期 `Attempts` 的错误诊断，整理成 3–6 个循序渐进的 `GuideSection`（从直觉定位到易混辨析）。
2. **按材料正文（`text`）生成**：将用户给出的笔记/段落/知识库主题摘要拆成 3–6 个 `GuideSection`，按 `<learner_profile>` 适配难度和类比，并允许精读结束后直接转出待确认卡片草稿（`Draft`）。

**Prompt 契约（`prompts/guide.md`）**：
- 使用 `<material>`（和可选 `<learner_profile>`）包裹不可信输入；指示模型按 JSON 返回：
  `{"title":"...","sections":[{"title":"...","intuition":"...","analogy":"...","key_points":["..."],"micro_check":{"question":"...","answer":"...","accepted":["..."]}}]}`
- 校验器对空标题、超长字符串、空要点列表做清洗与截断；若至少能产 1 个有效 `GuideSection` 才入库。
- **无模型降级（`Plain: true`）**：当 hub 模型未配置或调用失败、但存在对应 `topic` 卡片时，按 `topic` 下现有卡片生成简洁离线分节精读（卡片正面为要点标题、背面为要点与微测参考答案），不让页面卡死。

### 4.3 互动滑卡精读界面（`ui.go` + 新增 `ui_guide.go`）

1. 在 `ui.go` 新增页面模式与状态字段：
   - `modeGuide = "guide"`
   - `modeMap = "map"`
   - `pageState` 新增：`Guide string \`json:"g,omitempty"\``、`Section int \`json:"sec,omitempty"\``、`GuideChecked bool \`json:"gc,omitempty"\``、`GuideAns string \`json:"ga,omitempty"\``
   - `readState` 校验 `len(st.Guide) <= 64`、`st.Section >= 0 && st.Section < 20`、`utf8.RuneCountInString(st.GuideAns) <= 400`。
2. **分步互动精读 UI 流程**（面向手机单手阅读）：
   - 顶部进度条/标题：`第 K / N 节 · {Section.Title}`，用 `wire.TextStyleHeadline` + `fit()` 防越界；
   - 正文分段展示：**一句话直觉** → **生活/兴趣类比**（有则显示）→ **关键要点列表**；
   - **内嵌随堂微测**：展示 `MicroCheck.Question` + 单行输入框 / 查看参考答案按钮；提交后走本地宽松规范化比对（忽略大小写与常见中英文标点，匹配 `Answer` 或 `Accepted`）显示判定结果与标准要点；
   - **动作按钮**：`下一节` / `上一节`；最后一节提供一键**「做成待确认卡片」**（将各节核心要点或随堂题加入 `Drafts` 供确认，复用去重键避免重复）以及**「用深度辅导练这章」**（直接跳转到对应 `topic` 的 `modeTutor`）。

---

## 5. M3 · 掌握度着色知识导图（`Map` 模块）

### 5.1 分层数据聚合（复用 `mastery.go`）

直接复用现有 `masteryByTopic(a.sched, a.st, a.now())`、`scoreCard`、`topicLine` 与 `causeSummary`，不维护第二份统计口径：
- 根节点汇总：总知识点数、`weak`（<50）/ `shaky`（50–74）/ `solid`（≥75）/ `new`（未学）分布；
- 每个知识点分支：按掌握度最弱优先排序，带状态色调（`statusTone`：`weak` → `ToneDanger`，`shaky` → `ToneCaution`，`solid` → `TonePositive`，`new` → `ToneNeutral`），并展示该知识点的**高频错因标签**（`causeSummary`）与薄弱卡数量。

### 5.2 导图交互视图（`ui.go` / `ui_guide.go`）

- 在学习首页新增入口按钮：**「知识地图」**（进入 `modeMap`）与**「精读学习」**（进入 `modeGuide`）。
- **总览视图（`modeMap` 且 `st.Topic == ""`）**：
  - 顶部状态筛选沿用 `statusFilter(st.Status)`（全部 / 薄弱 / 不稳 / 掌握 / 未学）；
  - 分组列表按掌握等级直观呈现，每个知识点行使用 `row(title, subtitle, ...)` 严格限制条数在 `maxListRows`（100）内，避免大列表超过 `wire.ItemsMost`。
- **下钻焦点视图（`modeMap` 且 `st.Topic != ""`）**：
  - 展示该知识点的知识地图树状摘要（所属来源、近期正确率、错因统计）；
  - 列出该主题下每张卡片的掌握度分数与状态标签（截断至 `maxListRows`，附剩余数量提示）；
  - 提供两个主 CTA：**「先精读这个知识点」**（进 `modeGuide` 生成或打开该主题精读）与**「针对弱点做 Tutor 练习」**（进 `modeTutor`）。

---

## 6. MCP 工具、Skill、Prompt 与文档同步

1. **新增 MCP 工具（`tools.go`）**：
   - `study_guide`：入参 `{topic?: string, text?: string, title?: string}`，调用 `guide.go` 生成或复用近期同类精读，返回结构化分节内容（含 `guide_id`、`sections[]` 与引导语）；仅改变扩展自身状态。
   - `study_map`：只读工具，入参 `{status?: "weak"|"shaky"|"solid"|"new", topic?: string, limit?: int}`，返回按掌握度分层的知识导图摘要（不传 `topic` 返回全图分支概览，传 `topic` 返回该节点下的要点与卡片掌握分布）。
   - `persona-extension.json` 的 `agent_tools` 为 `"study_*"` 通配，新工具自动暴露。
2. **快捷入口与清单（`persona-extension.json`）**：
   - 版本升至 `"0.3.0"`；
   - 新增快捷提示词 `guide-weak`（标题 `"精读薄弱点"`，文本 `"看看我的知识地图，挑最薄弱的知识点先带我精读一遍并随堂小测。"`）。
3. **文档与 Skill 同步**：
   - 更新 `skills/quiz/SKILL.md`：记录 `study_map` 与 `study_guide` 的使用时机（「先带我理顺概念 / 看看我学到哪了」→ 先 `study_map` 再 `study_guide`，理解完再接 `study_practice` / `study_make_cards`）；
   - 更新 `docs/extensions/study.md` 与 `extensions/study/README.md`，保证与新配置项、界面入口和工具一致。

---

## 7. 测试、安全红线与验收标准

### 7.1 安全与健壮性要求
- **Prompt 围栏**：`<learner_profile>` 与 `<material>` 均需剥离大小写混淆的闭合标签（`</material>`、`</Material>` 等）后再拼装，并在 `guide_test.go` 做专门回归测试；
- **状态兼容与容错**：损坏 `guides` 纪录、`Section` 越界、空 `Sections`、超长 `Topic` / `Query` 在 UI 和工具中不得 panic，且能安全回退；
- **无外部权限扩张**：`persona-extension.json` 的 `permissions.hub`（`kb.read`, `models`, `notify`）与 `permissions.network` 保持不扩权。

### 7.2 必补测试与验证命令
1. 新增 `extensions/study/guide_test.go`：
   - 画像枚举/长度归一化与 XML 围栏剥离；
   - `study_guide`（`topic` 与 `text` 两条路径）生成、无模型离线降级、随堂微测判分与生成待确认草稿去重；
   - `study_map`（全局筛选与单主题下钻）只读返回校验；
   - 旧版 `stateVersion=1` 数据（无 `guides` 字段）加载兼容与 `maxGuides` 淘汰上限。
2. 扩展 `extensions/study/viewcheck_test.go`：
   - 使用 `wire.CheckView` 与 `CheckViewJSON` 覆盖 `modeGuide`（首节/中间节/末节/微测判答后/无模型降级）与 `modeMap`（空态/全局分层/单主题下钻/超长中文要点与 400+ 节点并发渲染）。
3. 交付前必须全部通过：
   ```bash
   cd extensions/study
   go vet ./...
   go test -race -count=1 ./...
   gofmt -l .
   go mod tidy -diff
   ```
   并在仓库根目录验证 manifest / docs / layout 检查：
   ```bash
   python3 -I scripts/check-docs.py
   python3 -I scripts/check-layout.py
   scripts/manifest.py check
   ```

### 7.3 Git 与协作约定
- 工作分支：`feat/study-learn-your-way`（基于包含 `0.2.0` 全量代码的 `feat/study-tutor`）。
- 提交作者：`s546126 <shengtao546@gmail.com>`；提交改动仅限 `s546126/persona-extensions` 的 `feat/study-learn-your-way` 分支，不向 `nwverse/persona-extensions` 直接推送或强合。
