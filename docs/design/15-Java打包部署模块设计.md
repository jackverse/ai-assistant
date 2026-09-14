# 15 — Java 打包部署模块（第三个功能入口）设计

> 需求：把 `bundle-packer` 的能力做成助手的第三个功能入口；Java 程序员部署用。
> 相比现有工具的两点改进：① **自动探测 Maven/JDK 路径**（不再手填）；
> ② **输出目录可在界面上选择**。
> 状态：第一版实现（本文档记录分析结论与实现边界）

---

## 1. 先分析现有工具（bundle-packer v2.2.2）

### 1.1 它是什么

```
bundle-packer-windows-amd64-v2.2.2.exe   11.7 MB
  go version -m 结果：
    path  bundle-packer
    go1.24.0
    dep   github.com/wailsapp/wails/v2  v2.12.0   ← 与助手同一套技术栈
    dep   git.sr.ht/~jackmordaunt/go-toast/v2     ← Windows 气泡通知
    dep   gopkg.in/yaml.v3                        ← 配置解析
    build -ldflags="-w -s -H windowsgui"          ← GUI 子系统
```

**结论：它本身就是 Go + Wails 桌面程序**，技术选型与助手完全一致——
说明这条路线在本机已被验证可行，助手把它收编为模块不会引入新风险。

### 1.2 配置格式（config.yaml）

```yaml
output_dir: "D:\\record\\security\\product-project\\product"   # 产物输出目录
base_path:  "D:\\code"                                        # 代码根目录
maven_path: "D:\\workplace\\apache-maven-3.9.6\\bin\\mvn.cmd"  # ← 手填
jdk_path:   "D:\\workplace\\jdk17"                             # ← 手填

projects:
  - name: "wic-sh"
    root: "wic-sh"                # 相对 base_path
    modules:
      - name: "wic-admin"         # type 缺省 = backend
      - name: "wic-pub"
      - name: "wic-admin-web"     # 前端模块
        type: frontend
        source: "dist"            # 取哪个目录当产物
        script: "admin build:prod"  # npm script 名（注意：名字里带空格！）
        output: "wic-admin-web"   # 产物名
```

### 1.3 从 exe 内嵌文案还原出的完整能力

| 能力 | 依据（提取到的文案） |
| --- | --- |
| 模块类型校验 | `必须是 backend 或 frontend（缺省为 backend）` |
| 构建模式 | `必须是 idea 或 maven（缺省为 maven）` |
| idea 模式需要本地仓库 | `本地仓库路径（idea 模式解析依赖时使用）` |
| **自行拼装 war_exploded** | `创建WEB-INF/classes`、`创建WEB-INF/lib`、`部署侧需自行保证 lib 完整）` |
| 模块选择 | `打包摘要：已选模块数与 prod 环境数`、`存在则默认选中` |
| 环境过滤 | 日志：`环境过滤：移除 WEB-INF/classes/application-dev.yml`（保留 prod） |
| 项目/模块拆分编辑 | `拆为项目 root（父目录）`、`拆为项目` |
| 输出目录操作 | `打开输出目录`、`产物输出目录` |
| 配置编辑器 | `打开配置编辑器`、`保存并刷新`、`参数已写入但重载配置失败` |
| 运行日志窗格 | `打开运行日志窗格：展示 bundle-packer.log 末尾并定位到最后` |
| 更新日志与版本检查 | `打开更新日志：解析 CHANGELOG.md`、`版本接口不可用时静默跳过` |
| 风险提示 | `代码未重新构建时会打出旧代码的包` |

### 1.4 实际运行证据（本机日志）

```
[wic-sh] mvn clean package 完成
[wic-sh/wic_admin_war_exploded] 环境过滤：移除 WEB-INF/classes/application-dev.yml …（9 个非 prod 文件）
[wic-sh/wic_admin_war_exploded] 正在压缩 → 已压缩 1442 个文件，耗时 3.2s
[pack] 耗时汇总 wic-sh / wic-admin：构建 118.0s + 压缩 3.7s，共 121.7s
[pack] 打包完成：成功=2 失败=0 总计=2
```

产物（`output_dir/product/`）：

| 产物 | 大小 |
| --- | --- |
| `wic_admin_war_exploded.zip` | 3.4 MB |
| `wic_pub_war_exploded.zip` | 3.0 MB |
| `wic-admin-web.zip` | 4.3 MB |
| `wic-pub-startup-web.zip` | 1.8 MB |

### 1.5 现有工具的短板（本次要改的）

| 短板 | 影响 | 本次处理 |
| --- | --- | --- |
| `maven_path` / `jdk_path` **必须手填**，且是绝对路径 | 换机器、升级 JDK 就要改配置；路径写错时报错晚（跑到构建才失败） | ✅ **自动探测**：扫 PATH、环境变量、常见安装目录、`D:\workplace` 等，列出候选让用户选 |
| `output_dir` 写死在配置里 | 想输出到别处必须手改 yaml | ✅ **界面选择目录**（Wails 原生选目录对话框） |
| JDK 与项目不匹配只能靠构建报错发现 | 例如项目要 JDK 17 而机器上默认 JDK 8 | ✅ 探测时**读取并展示 JDK 版本**，打包前校验并提示 |
| 前端 script 名含空格（`admin build:prod`）容易被手写坏 | 配置错误难以定位 | ✅ 界面**读取 package.json 列出可用 script** 供选择（下一版），本版先做校验提示 |
| 日志只能事后看 `bundle-packer.log` | 排查滞后 | ✅ **实时流式输出到界面** |

---

## 2. 模块设计

### 2.1 在助手架构中的位置

第三个功能入口，与扫描、清理平级（见 `14-主副页面与AI集成设计.md`）：

```
modules:
  chat       AI 助手        （默认启用）
  scan       磁盘扫描        （默认启用）
  deploy     Java 打包部署   （默认启用 ← 本次新增）
  clean      垃圾清理        （默认禁用，开发中）
  migrate    空间迁移        （默认禁用，规划中）
```

配置写在助手的 `config.yaml` 里（新增 `deploy` 节），**不再需要单独的 bundle-packer 配置文件**。

### 2.2 打包流程（v1 实现的范围）

```
后端模块（backend）
  1. 定位模块根目录（含 pom.xml，否则报错）
  2. mvn clean package            ← 用探测到的 mvn.cmd + JAVA_HOME
  3. 取产物：
       优先 target/*.war → 解压得 WEB-INF/{classes,lib}（maven 的权威产物，最可靠）
       否则若有 target/classes → 回退为 classes + 依赖拷贝（并明确告警 lib 可能不全）
  4. 环境过滤：删除 WEB-INF/classes/application-*.yml 中非选定环境的
       （保留 application.yml 与 *-<env>.yml）
  5. 压缩为 <output>_war_exploded.zip

前端模块（frontend）
  1. 定位模块根目录（含 package.json）
  2. npm run "<script>"          ← script 名可能含空格，必须整体加引号
  3. 校验 source 目录（默认 dist）存在，否则报错并提示先构建
  4. 压缩 source 目录为 <output>.zip
```

**与 bundle-packer 的一处刻意差异**：它自己拼 `WEB-INF/classes` + `WEB-INF/lib`；
本模块优先**解压 maven 产出的 war**——war 是 maven 的权威产物，lib 完整性由构建保证，
比手工拼装更不容易出错。当项目不产出 war 时才回退到拼装路径，并在日志里明确告警。

### 2.3 工具链自动探测（本次重点）

探测顺序（先快后慢，带缓存）：

**Maven**
1. `MAVEN_HOME` / `M2_HOME` 环境变量 → `<dir>\bin\mvn.cmd`
2. `PATH` 中的 `mvn.cmd`（`where mvn` 的等价实现：遍历 PATH 各段）
3. 常见安装根下的一级目录：`C:\Program Files\apache-maven-*`、`D:\workplace\apache-maven-*`、
   `D:\dev\*`、`%USERPROFILE%\scoop\apps\maven\*`、`%USERPROFILE%\.m2`（只用于判断存在）
4. 从 `D:\code` 之类代码根扫 `mvnw.cmd`（wrapper，项目自带，优先于全局 mvn）

**JDK**
1. `JAVA_HOME` → `<dir>\bin\java.exe` + `javac.exe`
2. `PATH` 中的 `java.exe`（并回溯到 JAVA_HOME 形态的上级目录）
3. 常见安装根：`C:\Program Files\Java\jdk*`、`C:\Program Files\Eclipse Adoptium\*`、
   `C:\Program Files\Microsoft\jdk*`、`D:\workplace\jdk*`、`D:\dev\*`、`%USERPROFILE%\.jdks\*`
4. 读取 `<dir>\release` 文件里的 `JAVA_VERSION`（JDK 9+ 都有这个文件，比跑 `java -version` 快且不启进程）

探测结果：
- 每个候选带**来源**（哪个规则找到的）与**版本**，界面按版本倒序列出，默认选版本最高的
- 每个候选做**可用性校验**（文件存在 + `bin\mvn.cmd` / `bin\javac.exe` 存在），不可用的标记出来
- **没有探测到时**给出人话提示（例如「JAVA_HOME 指向不存在的目录」），而不是让构建慢慢失败

### 2.4 输出目录

- 界面提供「选择目录」（Wails `runtime.OpenDirectoryDialog` 原生对话框）
- 记录最近使用过的目录，默认选中
- 输出目录不存在时**先创建**再打包（并与「产物输出」分开处理，避免把源目录写坏）
- 打包完成后提供「打开输出目录」按钮（`explorer.exe`）

### 2.5 实时日志与进度

- 打包在后台 goroutine 执行，逐行读取子进程的 stdout/stderr
- 通过 Wails 事件 `pack:log`（每行）与 `pack:state`（阶段与进度）推给界面
- 阶段：`准备 → 构建(mvn/npm) → 组装(war 解压) → 环境过滤 → 压缩 → 完成`
- 支持取消（杀掉子进程树；Windows 上需要 `taskkill /T /F`，只杀父进程会留下 mvn 的 java 子进程）

### 2.6 安全边界（与其它模块一致的原则）

| 原则 | 落地 |
| --- | --- |
| 只写输出目录与新生成的构建产物 | 不碰源码目录里的其它文件；删除操作只发生在解压出来的临时组装目录内 |
| 环境过滤只删组装目录里的 `application-*.yml` | **绝不动源码目录里的配置文件**（这是最关键的一条：误删源码里的 prod 配置是不可接受的） |
| 执行外部命令有白名单 | 只执行探测到的 `mvn.cmd` / `mvnw.cmd` / `npm.cmd` / `node.exe`；参数由配置与界面固定生成，不接受自由文本拼接 |
| 取消与超时 | 用户可随时取消；子进程树整体终止 |

---

## 3. v1 实现范围与后续

### 本版实现

- ✅ 工具链自动探测（Maven/JDK，含来源与版本，可手选）
- ✅ 输出目录界面选择 + 记住上次 + 打开目录
- ✅ 模块列表与勾选（来自配置，逗号分隔或 yaml 皆可）
- ✅ 后端打包：`mvn clean package` → war 解压 → 环境过滤 → zip
- ✅ 前端打包：`npm run "<script>"` → 压缩 dist → zip
- ✅ 实时日志流 + 阶段进度 + 取消
- ✅ 助手内配置持久化（`config.yaml` 的 `deploy` 节）

### 后续（不在本版）

- ⬜ `idea` 构建模式（用 IDEA 的编译输出 + 本地仓库解析依赖）——需要理解 IDEA 的 module 输出布局
- ⬜ 从 `package.json` 读取 script 列表供选择（避免手写 script 名）
- ⬜ 多环境矩阵（一次产出 prod + test 两套）
- ⬜ 产物清单与校验和（SHA256），便于部署侧核对
- ⬜ 打包含源码版本号/commit 注入到产物内的版本文件
