# 21 — Java 开发模块：把「打包部署」扩成「开发时的运行栏」

> 需求原话：*java 部署可以稍微改一下，改成 JAVA 开发……可以单独启动停止服务，
> 也可以加载依赖，也可以看 git 文件修改，类似于简化版的 idea，
> 但是功能不要过重，先完成端口服务启停，如果端口被占用，可以杀端口。*
> 状态：已实现并实测（服务启停 / 端口归属与杀端口 / 依赖加载 / Git 变更）

---

## 1. 定位：是运行栏，不是 IDE

「简化版 IDEA」如果按字面理解会掉进一个无底洞（索引、补全、调试器、重构……）。
真正每天都要做的其实只有四件事，本模块只做这四件：

| 要做的事 | 界面位置 | 底层动作 |
| --- | --- | --- |
| 把某个模块跑起来 / 停下来 | 「服务」页签 | 在模块目录里执行一条命令，管住整棵进程树 |
| 端口被占了，得先清掉 | 服务卡片 + 端口工具 | 查 TCP 表拿到占用 PID，确认后终止 |
| 依赖没拉齐，编译报错 | 服务卡片「加载依赖」 | `mvn dependency:go-offline` / `npm ci` 系 |
| 我改了哪些文件、提交了没 | 「变更」页签 | `git status --porcelain -z` 只读查询 |

**明确不做**：不索引代码、不做补全与跳转、不提供提交/切分支/拉取推送。
后一组是**有风险**的操作，不该藏在一个顺手点开就能点到的地方。

## 2. 关键设计决策

### 2.1 复用打包模块的项目清单，不另配一套

用户已经在「配置」页扫过一遍代码目录、确认过项目与模块。开发页直接复用这份
`deploy.projects`，只在 `ModuleConfig` 上加了两个字段：

```yaml
run: mvn spring-boot:run     # 启动命令，留空 = 这个模块不由本工具启动
port: 8081                   # 期望端口，用于查占用与杀端口
```

代价是「部署配置」里混进了开发态字段，收益是用户永远不用配两遍——
这是划算的，因为扫描出的模块清单本来就是同一份东西。

### 2.2 启动命令与端口靠探测，不靠用户手抄

新增 `internal/deploy/devcfg.go`，全部基于模块里既有的文件推断：

| 模块类型 | 启动命令 | 端口来源 |
| --- | --- | --- |
| 后端 | pom.xml 含 `spring-boot-maven-plugin` → `mvn spring-boot:run` | `src/main/resources/application*.yml` 的 `server.port`（优先 `spring.profiles.active` 指向的那个文件；`application.properties` 与 `*.yaml` 也认） |
| 前端 | package.json 里挑 dev / serve / start 脚本 → `npm run "admin dev"` | `vite.config.*` 的 `server.port`、`vue.config.*` 的 `devServer.port`，再退到 `.env*` 里的 `PORT` |

两个踩过的坑写进了实现：

- **脚本名可能含空格**（本机实测叫 `admin dev`），命令必须写成 `npm run "admin dev"`，
  否则 npm 会把 `dev` 当成传给脚本的参数。
- **`port:` 只在 `server:` 块里找**。实测的 `application-dev.yml` 里还有
  `spring.redis.port`、`management.server.port` 等多处 `port`，全局搜会取到 6379——
  用户点启动时就会去杀错端口。所以按缩进界定 `server` 块的范围。

探测是**显式动作**（卡片右上角「识别启动方式」），不在启动时静默填充：
用户如果把命令清空（例如他习惯用 IDEA 跑），启动时又给填回来，就是程序跟用户较劲。

### 2.3 进程编排：三层进程树 + 不弹黑窗

`mvn spring-boot:run` 实际是 `cmd.exe → mvn.cmd → java.exe` 三层。停止时必须
`taskkill /T /F` 连子树一起杀，否则会留下 java 继续占着端口——
用户看到的现象是「服务停了但端口还被占」。

子进程一律 `CREATE_NO_WINDOW + HideWindow`：本程序是 GUI 子系统，不加这个标志，
启动服务会弹一堆 cmd 黑窗（打包模块踩过同一个坑）。

环境注入 JDK 的 `bin` 与 Maven 的 `bin` 目录：配置里存的是 `mvn.cmd` 全路径，
而命令里写的是朴素的 `mvn`——把它的目录前置到 PATH，两者就对上了。

### 2.4 端口归属用 API，不用 netstat

`GetExtendedTcpTable(TCP_TABLE_OWNER_PID_ALL)` 一次调用就能拿到全部连接的
本地地址、端口、状态与 owning PID：

- 起进程跑 `netstat -ano` 要几百毫秒，界面每次刷新端口状态都等它，是可感的卡顿；
- netstat 的列宽与表头随系统语言变化，解析规则很脆。

两个字节序细节容易写错（实现在 `internal/winapi/tcp.go`）：表里端口按网络序
存在 DWORD 低 16 位，读出来要翻半字节；IPv4 地址同理。有单测钉住。

### 2.5 「杀端口」不能有强制模式

终止进程是破坏性动作，所以：

- 界面先弹确认框，**列出 PID、进程名、完整路径、监听状态**——点下去之前必须知道会杀掉谁；
- 系统关键进程有硬白名单（`svchost` / `lsass` / `winlogon` / `explorer` / `System` …），
  一律跳过并在结果里说明原因；
- **刻意不提供 `force` 开关**。开关一旦存在，迟早会被用成「一键杀干净」。

### 2.6 退出时停止由本程序启动的服务

不这么做的话，用户关掉助手后 java 进程还占着 8080，下次开发被「端口已被占用」
绊住，而他完全不记得是谁占的。因此在 `OnShutdown` 里 `StopAll`。
界面上也写明了这一点。

### 2.7 Git 查询保持只读

- `GIT_OPTIONAL_LOCKS=0`：默认 `git status` 会刷新并回写索引，对「只是看一眼」
  来说是不必要的写操作，关掉它才是真只读。
- `--porcelain=v1 -z`：文件名可能含空格、引号、换行，按行拆会把一个改动拆成两行；
  重命名条目后面还紧跟一个原路径字段，必须吃掉。
- `core.quotepath=false`：否则中文文件名会变成 `\344\270\255` 这种八进制转义。
- `--no-pager`：否则 git 可能挂在分页器上等输入，界面永远转圈。
- 行数统计用 `git diff --numstat HEAD`（一次调用覆盖已暂存 + 未暂存）。

### 2.8 绑定边界上只用 main 包 DTO

`javadev_api.go` 把所有跨边界的类型都转成 main 包的 DTO，不直接把
`internal/javadev` 的类型当返回值。原因在 `app.go` 里记过：绑定方法签名
引用别的包的类型时，Wails 的运行时绑定会**静默丢弃整个方法**，
前端调用时报 `is not a function`，且没有任何报错。多写一层转换，换掉一整类问题。

## 3. 实测记录（本机真实工程 D:\code\wic-sh）

点一次「识别启动方式」，6 个模块全部识别正确，并且把本机正在跑的进程认了出来：

| 模块 | 识别到的命令 | 识别到的端口 | 界面显示 |
| --- | --- | --- | --- |
| wic-admin | `mvn spring-boot:run` | 8081 | 被占用：java.exe (PID 18956) |
| wic-pub | `mvn spring-boot:run` | 8080 | 被占用：java.exe (PID 23564) |
| wic-task | `mvn spring-boot:run` | 8089 | 空闲 |
| wic-admin-web | `npm run "admin dev"` | 8001 | 被占用：node.exe (PID 40836) |
| wic-pub-startup-web | `npm run wic-startup-dev` | 5173 | 被占用：node.exe (PID 41384) |
| wic-pub-westart-web | `npm run "westart dev"` | 5174 | 被占用：node.exe (PID 27674) |

三个后端端口各不相同（8080/8081/8089）、三个前端脚本命名毫无规律，
说明这套探测面对真实工程是有效的。Git 页签在同一仓库上正确读出
`master` 分支、24 处改动与逐文件 `+N −M`，未跟踪/修改分类正确。

## 4. 边界与后续

- **不做端口转发、不做多实例端口分配**：一个模块一个端口，够用。
- **不做日志搜索/过滤**：2000 行环形缓冲 + 最近 80 行预览，够定位启动失败。
- **不做 .cmd/.bat 之外的 shell 适配**：Windows-only 工具，命令统一走 `cmd.exe`。
- 若将来要支持「一个模块多套启动配置」（如不同 profile），再加一层即可，
  当前 `run` 是单条命令，用户也可以直接在命令里写 `-Dspring-boot.run.profiles=test`。
