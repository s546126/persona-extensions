---
name: disklessly
description: 管用户每台电脑的硬盘空间（经本机 disklessly MCP 服务器）：运行 Persona 的电脑和 ~/.persona/config.json、~/.ssh/config 里能远程登录的电脑。看每台电脑硬盘用了多少（用到 80% 算偏满、90% 算快满了，剩得太少也算），找出放心删的缓存和旧日志、建议先看的大东西，把一周没动的 AI 对话（Claude Code、Codex 等）搬到网盘、要用时拿回来。用户说「帮我腾点空间」「哪台电脑快满了」「硬盘不够了」时先调用只读的 mcp__disklessly__disklessly_advise，讲前几步腾出多少、有什么风险，问做哪一步。用户想找回一段旧对话、只说得出大概内容和时间时，调用只读的 mcp__disklessly__disklessly_sessions_find，给用户看最像的几个，选定后再用 disklessly_sessions_restore 拿回来。只看的工具（disklessly_machines_status / disklessly_scan / disklessly_history / disklessly_sessions_search / disklessly_sessions_status / disklessly_sessions_find / disklessly_advise）直接运行；删除、搬到网盘、拿回来、装配套小工具和改电脑列表都要用户在对话里确认。
---

# Disklessly 硬盘空间（mcp__disklessly__* 工具）

跟用户说话时用页面上的说法：**电脑**、**硬盘用了多少**、**放心删** / **建议先看**、**先看看会删什么**、**清理记录**、**一周没动的 AI 对话**、**搬到网盘** / **拿回来**、**网盘**、**配套小工具**（disklessly 命令行程序）。不要对用户说「水位」「冷会话」「上云」「备份后端」「Dry-Run」「CLI」这些词；工具名和参数照下面原样用。

## 能删什么

一台电脑「能删什么」分这几种（括号里是工具参数里的种类 ID）：

- Go 的编译和下载缓存（`go-cache`）：`go-build` 编译缓存、`go/pkg/mod` 下载的依赖包。
- Node 和 Python 下载的包（`node-python-cache`）：npm、pnpm、yarn、pip、uv 存的包。
- Docker 没在用的镜像和构建缓存（`docker-unused`）：没有名字、没有任何容器用到的旧镜像，没在用的构建缓存。
- Rust 和 Xcode 的编译缓存（`rust-xcode-build`）：Cargo 下载的依赖、Xcode DerivedData、SwiftPM、Clang、Gradle；Playwright 自动测试用的浏览器属于「建议先看」。
- 系统日志和过期的临时文件（`tmp-logs`）：系统日志里 7 天以前的部分、系统换下来的旧日志、临时文件夹里编译和安装剩下的文件和放了 7 天以上的文件。**删了就没有了**，最近 7 天的系统日志会留着。
- 一周没动的 AI 对话（`cold-agent-sessions`）：超过 7 天没动的 Claude Code、Codex 等对话。不是删，是**搬到网盘**：由那台电脑上的配套小工具先放进网盘、读回核对无误才删电脑上的那份；没装配套小工具或网盘连不上时，一个都不搬、不删。

「放心删」是能重新生成或重新下载的缓存，以及过期的旧日志和临时文件；「建议先看」是比较大、重新下载很慢的东西（比如 Playwright 的浏览器），默认不删。

**绝不碰**：正在运行的服务和容器、项目文件和还没提交的改动、最近 7 天用过 / 正打开着 / 还有程序在用的 AI 对话、密码和密钥（`~/.ssh`、`~/.gnupg`、`~/.persona` 等）。删缓存时还会检查有没有正在运行的编译工具（`go`、`pnpm`、`cargo`、`swift` 等）和 `/proc/*/maps` 映射，正在用的那一项自动跳过。

## 帮用户腾空间（「帮我腾点空间」「哪台电脑快满了」「硬盘不够了」）

1. 调用 `disklessly_advise {machine?, goal?, refresh?}`（只读，不删不搬，也不读对话内容）：
   - 用户点名哪台电脑就填 `machine`；说「最满的那台」就填 `goal: {fullest: true}`；说「腾出 20 G」就填 `goal: {free_gib: 20}`（够了就不再往下列）。
   - 默认用上次检查的结果；每台电脑的 `checked` 说是多久以前检查的，太旧（`stale`）或用户要最新的，就用 `refresh: true`（和页面上的「检查」一样，只读取大小）。
2. 先用一两句话说哪台电脑最满（`summary`、`machines`），再按 `steps` 的顺序列前三步左右（现在就能做的排在前面，`possible` 为 false 的都在后面，和页面上的顺序一样）：每步说 `what`（做什么）、`frees`（大约腾出多少）、`risk`（风险，一句话）；`possible` 为 false 的说 `blocked`（为什么现在做不了）和 `unblock`（怎么才能做）。「建议先看」(`look_first`) 单独说，默认不删。需要时提一句 `not_touched` 里哪些不会动。
3. 问用户做哪一步（可以选几步）。**用户没选的步骤不做**，不要自己一口气全做。
4. 对用户选的每一步，照那一步 `call` 里的工具和参数原样调用（`disklessly_clean {machine, items}` 或 `disklessly_sessions_offload {machine}`）；这是写入工具，Persona 会请用户确认，用户不同意就停。搬对话前可以先用 `preview_call`（`dry_run: true`）给用户看会搬哪些、哪些因为还在用会留着。
5. 每做完一步，按工具回答里的实测数告诉用户**真正**腾出了多少（`record.summary`，比如 `5.4 GiB → 18.0 GiB (+12.6 GiB)`，或 `record.freed_bytes`），不要只报预计数；没做成就照实说原因。
6. 做不了的步骤：`unblock_call` 是能帮上忙的工具（比如 `disklessly_cli_install` 装配套小工具，它也要用户确认；`disklessly_sessions_status refresh=true` 重新检查网盘），`command` 是要在那台电脑上运行的命令，原样给用户。

页面上也有同样的步骤：「硬盘空间」页最上面的「帮我腾空间」，每一步写着用哪个按钮（`page_buttons`）。

## 帮用户找回一段旧对话（「帮我找回上次聊 X 的那段对话」「上周那个修 Y 的会话还在吗」）

1. 从用户的话里提取线索，调用 `disklessly_sessions_find`（只读，不读对话内容、不拿回任何东西）：
   - `keywords`：说的是什么（几个词；同一个意思可以中英文各给一个，比如「购物车」和「cart」）；默认 `match: "any"`，用户说得很确定时才用 `"all"`。
   - `after` / `before`：时间（`2026-11-02` 这样的日期，按对话最后改动的时间算）。「上周」「月初」这类说法换算成日期，宁可放宽几天。
   - `tool`（Claude Code、Codex…）、`project`（项目名或工作文件夹里的词）、`machine`（哪台电脑）、`location`（`on_computer` 还在电脑上 / `on_drive` 已经搬到网盘）：用户提到才填。
2. 把前三个左右的候选给用户看：`title`（和页面一样的标题，不报编号）、`when`、`machine_name`、`location`（在哪），必要时加 `project`、`why`。问是哪一个。
3. 一个都没找到时，照 `too_narrow`（哪个条件太窄、去掉能找到几个）和 `suggest` 请用户补充或放宽，**不要自己猜一个**；需要搜对话内容里的词时，可以用 `disklessly_sessions_search {query, deep: true}`。
4. 用户选定后，照那个候选的 `restore_call` 调用 `disklessly_sessions_restore {machine, id}`（会把对话从网盘拿回原来的位置，Persona 会请用户确认）。`can_restore` 为 false 的不调用，把 `note` 告诉用户（还在电脑上就不用拿回来；别的设备上的要在那台设备上拿）。
5. 拿回来后，把回答里的继续命令（`resume`，比如 `claude --resume <id>`）原样给用户，告诉他在哪台电脑上运行。

「AI 对话」页的搜索框也能输几个词、加「昨天」「上周」「上个月」这样的时间。

## 只看的工具（直接运行）

1. `disklessly_machines_status {refresh?}`：所有电脑硬盘用了多少（`ok` 宽裕 / `warning` 偏满 / `critical` 快满了 / `offline` 连不上）、还剩多少、能腾出多少、AI 对话的情况和已经腾出的空间。`refresh: true` 时重新读 `~/.persona/config.json` 和 `~/.ssh/config` 里新加的电脑。
2. `disklessly_scan {machine?, cached?}`：检查一台电脑（如 `local`、`build-box`、`nas-box`）：硬盘用了多少、每种能删的东西各多少、AI 对话的情况，和「先看看会删什么」的清单（`dry_run_plan`）。只读取大小。
3. `disklessly_history {machine?, limit?}`：清理记录：每次删之前和之后量的剩余空间（如 `5.4 GiB → 18.0 GiB (+12.6 GiB)`）。
4. `disklessly_advise {machine?, goal?, refresh?}`：腾空间的步骤，见「帮用户腾空间」。
5. `disklessly_sessions_find {keywords?, match?, after?, before?, tool?, project?, machine?, location?, limit?}`：按描述找旧对话，见「帮用户找回一段旧对话」。
6. `disklessly_sessions_search {query, machine?, limit?, deep?}`：用户想找回以前的 AI 对话时，按标题、第一句话、项目、工作文件夹、关键词搜索搬到网盘的对话；本地记下的找不到时再用 `deep: true` 在电脑上搜对话摘要（较慢）。结果里的位置：`在网盘里`（最近 24 小时检查过、能拿回来）、`网盘里找不到`、`还在电脑上`、`暂时不确定`（网盘连不上或还没检查，不代表丢了）。
7. `disklessly_sessions_status {machine?, refresh?}`：每台电脑能不能把对话搬到网盘、网盘连不连得上、记下了多少对话、各在哪。要最新的就 `refresh: true`。每台电脑的 `state`：`ready` 正常、`no_cli` 还没装配套小工具、`cli_too_old` 配套小工具太旧、`no_backend` 还没设置网盘、`backend_unreachable` 网盘连不上、`unreachable` 连不上这台电脑、`unreadable` 配套小工具出错了、`unchecked` 还没检查。跟用户说时用返回的 `message`（哪台、为什么）和 `next`（下一步），有 `command` 就原样给；`detail` 是原始输出，用户要看细节时再给。配套小工具没装或太旧时，先用 Homebrew：`fix` 指向 `disklessly_cli_install`（`method=brew` 运行 `brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli`，已经用 Homebrew 装过就升级），用户确认后再调用；那台电脑没有 Homebrew 时才用 `method=copy` 从同系统、同架构的电脑复制，都不行就把 `command` 给用户自己运行。

## 会改东西的工具（都要用户确认）

先给用户看要删什么、大约腾出多少（`disklessly_advise` 的步骤，或 `disklessly_scan` 的 `dry_run_plan`），用户同意了再调用：

- `disklessly_clean {machine?, categories?, items?, include_review?, dry_run?, exclude_sessions?}`：
  - 不填 `categories` 和 `items` 时只删放心删（`safe`）的；要删建议先看（`review`）的（比如 Playwright 的浏览器），传 `include_review: true` 或在 `items` 里点名。注意：填了 `categories` 时，那几类里建议先看的也会删；只想删放心删的就用 `items` 点名（`disklessly_advise` 给的 `call` 就是这样）。不填时不包括搬对话。
  - 一周没动的 AI 对话只有明确传 `categories: ["cold-agent-sessions"]` 或点名对话那一项时才搬，一次覆盖所有 AI 助手（Claude Code、Codex 等）。更推荐用 `disklessly_sessions_offload`；还在用、必须留在电脑上的对话放进 `exclude_sessions`（对话文件的绝对路径或会话 ID）。网盘连不上时会报错，一个都不搬、不删。
  - 返回删之前和之后量的剩余空间，记进清理记录。
- `disklessly_sessions_offload {machine, dry_run?, exclude_sessions?}`：把这台电脑上 7 天没动的 AI 对话交给配套小工具搬到网盘，读回核对无误才删电脑上的那份。正在运行的程序（`--resume`、`codex resume`）、正打开着的对话文件、herdr / Persona 面板还在用的对话自动留着（开始前先查一次写进排除清单；配套小工具每删一个之前还会重读清单、重新检查运行中的程序和 herdr 的 session.json）；查不清哪些还在用或网盘连不上时中止，一个都不删。先用 `dry_run: true` 给用户看会搬哪些、哪些因为还在用会留着。
- `disklessly_sessions_restore {machine, id}`：把一个搬到网盘的对话拿回来，放回原来的位置，返回接着聊的命令（`claude --resume <id>` 或 `codex resume <id>`；别的 AI 助手没有这个命令），告诉用户在那台电脑上运行。别的设备上的对话不会拿回到这台电脑，要在原来那台设备上拿回来。
- `disklessly_machine_upsert {host, id?, name?, user?, port?, identity_file?}`：添加或修改一台能远程登录（SSH）的电脑。
- `disklessly_machine_delete {machine}`：不再管一台远程电脑（运行 Persona 的电脑 `local` 不能移除；电脑上的文件不会动）。
- `disklessly_cli_install {machine, method?}`：给没装或版本太旧的电脑装 / 升级配套小工具（优先 Homebrew），装好后运行 `disklessly version` 确认，再重新读这台电脑的对话情况。会在那台电脑上装软件。
- `disklessly_files_offload {machine, paths, dry_run?}`：把用户点名的文件、文件夹或 App（`/Applications` 或个人文件夹里的 `.app`）搬到网盘：文件和文件夹由 `disklessly offload` 上传、原处留下 `.disklessly` 占位文件，App 由 `disklessly pack` 打包、留下替身 App，读回核对无误才删电脑上的原件，返回每一项的结果和实测腾出的空间。先用 `dry_run: true` 检查（个人文件夹以外、标准文件夹本身、资源库、App 包里面、符号链接、密钥文件夹、已经有占位文件的路径会被拒绝，并说明原因），把能搬的和大约多大告诉用户，确认后再执行。配套小工具没装或太旧时它会报错，不会自己装：先说清楚要装什么、装在哪台电脑，用户确认后用 `disklessly_cli_install`。拿回搬走的文件：在那台电脑上运行 `disklessly fetch <占位文件>`。
