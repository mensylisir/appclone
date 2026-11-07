# ENV.md

## 开发环境与语言运行时管理规范

## 1. 总体原则

所有开发任务必须遵循统一的运行时版本管理规范。

**严禁将操作系统自带的 Go、Python、Ruby、Node.js 和 Java 作为项目开发、构建、测试、调试及运行环境。**

上述语言的所有运行时必须通过指定的版本管理工具安装、管理、切换和使用。

指定工具如下：

| 语言 | 版本管理工具 | 命令 |
|---|---|---|
| Go | GVM | `gvm` |
| Python | pyenv | `pyenv` |
| Ruby | RVM | `rvm` |
| Node.js | NVM | `nvm` |
| Java | SDKMAN! | `sdk` |

必须严格遵守以下原则：

1. 不使用系统自带的语言运行时。
2. 不使用系统包管理器安装的语言运行时作为项目运行环境。
3. 不绕过指定的版本管理工具。
4. 项目有明确版本要求时，必须使用符合要求的版本。
5. 目标版本未安装时，必须通过对应的版本管理工具安装。
6. 不得因为安装失败或环境异常而退回系统版本。
7. 不得擅自修改用户的全局默认版本。
8. 不得擅自破坏、覆盖或重建已有的开发环境。


## 开发环境规范（强制遵循）

开始任何开发、构建、测试、调试或部署任务之前，必须先阅读并遵循项目根目录下的 `ENV.md`。

`ENV.md` 是本项目开发环境与运行时管理的权威规范，规定了语言运行时、版本管理工具、版本选择、环境初始化、依赖管理和 PATH 配置等要求。

必须遵守以下规则：

1. **优先阅读：** 每次开始任务时，先检查并完整阅读 `ENV.md`。
2. **严格执行：** 所有开发操作必须符合 `ENV.md` 中的环境管理规范。
3. **版本检查：** 执行项目命令前，检查项目要求的语言版本及当前实际使用的运行时。
4. **自动安装：** 如果当前版本不满足项目要求，必须使用 `ENV.md` 指定的版本管理工具安装并切换到符合要求的版本。
5. **禁止绕过：** 不得使用系统自带的语言运行时、其他版本管理工具或临时替代方案绕过 `ENV.md` 的规定。
6. **环境修复：** 如果版本管理工具或 Shell 环境异常，必须优先修复环境，不得退回系统运行时。
7. **保护全局配置：** 不得擅自修改全局默认版本、卸载已有版本或覆盖用户的环境配置。
8. **持续遵循：** 整个任务执行过程中，包括子任务、脚本、构建、测试和依赖安装，都必须遵循该规范。

如果 `ENV.md` 不存在、无法读取或规范之间存在冲突，必须暂停相关开发操作并向用户说明，不得自行忽略或猜测环境规范。

**优先级：** `ENV.md` 中的环境管理规范是强制要求，不能因为 Agent 自身的默认配置、工具习惯或项目脚本而绕过。

## 2. 运行时管理规范

### 2.1 Go

Go 必须使用 GVM 管理。

- 使用 `gvm list` 查看已安装版本。
- 使用 `gvm current` 查看当前版本。
- 使用 `gvm use <version>` 切换版本。
- 使用 `gvm install <version>` 安装新版本。
- 使用 `go version` 验证实际运行版本。
- 使用 `which go` 检查实际执行路径。

禁止：

- 使用 `/usr/bin/go`。
- 使用系统包管理器安装的 Go 作为项目运行时。
- 未检查 GVM 环境就直接执行 Go 项目命令。

如果项目要求的 Go 版本未安装：

1. 检查项目的 `go.mod` 中的 `go` 和 `toolchain` 声明。
2. 确认所需 Go 版本。
3. 使用 GVM 安装所需版本。
4. 切换到目标版本。
5. 验证 `which go` 和 `go version`。
6. 确认使用的是 GVM 管理的 Go 后再继续工作。

注意：Go 的 `go.mod` 中的 `go` 声明与 `toolchain` 声明含义不同，不得混淆。

### 2.2 Python

Python 必须使用 pyenv 管理。

- 使用 `pyenv versions` 查看已安装版本。
- 使用 `pyenv version` 查看当前版本。
- 使用 `pyenv install <version>` 安装新版本。
- 使用 `pyenv local <version>` 设置项目版本。
- 使用 `pyenv shell <version>` 设置当前 Shell 会话版本。
- 使用 `python --version` 验证版本。
- 使用 `which python` 检查实际执行路径。

禁止：

- 使用 `/usr/bin/python`。
- 使用 `/usr/bin/python3`。
- 使用系统自带 Python 执行项目代码。
- 使用系统 Python 创建项目虚拟环境。
- 未确认解释器来源就执行 `pip install`。

如果项目要求的 Python 版本未安装：

1. 检查 `.python-version`。
2. 检查 `pyproject.toml` 中的 `requires-python`。
3. 检查 `Pipfile`、`runtime.txt`、`setup.py` 等版本声明。
4. 确认所需 Python 版本。
5. 使用 pyenv 安装目标版本。
6. 使用 `pyenv local <version>` 固定项目版本，或者在当前 Shell 中使用 `pyenv shell <version>`。
7. 验证 Python 解释器路径和版本。
8. 确认无误后再创建虚拟环境和安装依赖。

Python 虚拟环境必须基于 pyenv 管理的 Python 创建。

推荐：

```bash
python -m venv .venv
source .venv/bin/activate
python --version
which python
```

激活虚拟环境后，必须确认其基础解释器来自 pyenv 管理的 Python。

### 2.3 Ruby

Ruby 必须使用 RVM 管理。

- 使用 `rvm list` 查看已安装版本。
- 使用 `rvm current` 查看当前版本。
- 使用 `rvm install <version>` 安装新版本。
- 使用 `rvm use <version>` 切换版本。
- 使用 `ruby --version` 验证版本。
- 使用 `which ruby` 检查实际执行路径。

禁止：

- 使用 `/usr/bin/ruby`。
- 使用系统包管理器安装的 Ruby 作为项目运行时。
- 未确认 RVM 环境就执行 Ruby 项目命令。
- 擅自修改 RVM 全局默认版本。

如果项目要求的 Ruby 版本未安装：

1. 检查 `.ruby-version`。
2. 检查 `Gemfile` 中的 Ruby 版本要求。
3. 使用 RVM 安装目标版本。
4. 使用 `rvm use <version>` 切换版本。
5. 验证 Ruby 路径和版本。
6. 确认使用 RVM 管理的 Ruby 后再执行项目操作。

如果 RVM 提示 PATH 配置异常，必须优先修复 Shell 初始化和 PATH 顺序，不得忽略警告继续使用不确定的 Ruby 环境。

### 2.4 Node.js

Node.js 必须使用 NVM 管理。

- 使用 `nvm list` 查看已安装版本。
- 使用 `nvm current` 查看当前版本。
- 使用 `nvm install <version>` 安装新版本。
- 使用 `nvm use <version>` 切换版本。
- 使用 `node --version` 验证版本。
- 使用 `which node` 检查实际执行路径。

禁止：

- 使用 `/usr/bin/node`。
- 使用系统包管理器安装的 Node.js 作为项目运行时。
- 使用系统自带 npm 执行项目依赖安装。
- 未确认 NVM 环境就执行 npm、npx、pnpm、yarn 等命令。

如果项目要求的 Node.js 版本未安装：

1. 检查 `.nvmrc`。
2. 检查 `.node-version`。
3. 检查 `package.json` 中的 `engines` 声明。
4. 确认项目要求的 Node.js 版本。
5. 使用 NVM 安装目标版本。
6. 使用 `nvm use <version>` 切换版本。
7. 验证 Node.js 和 npm 的实际路径及版本。
8. 确认使用 NVM 管理的 Node.js 后再执行项目操作。

对于使用 pnpm、yarn 等包管理工具的项目，也必须确保它们运行在 NVM 管理的 Node.js 环境下。

### 2.5 Java

Java 必须使用 SDKMAN! 管理。

- 使用 `sdk list java` 查看可用版本。
- 使用 `sdk current java` 查看当前版本。
- 使用 `sdk install java <version>` 安装新版本。
- 使用 `sdk use java <version>` 切换当前 Shell 会话版本。
- 使用 `sdk default java <version>` 设置全局默认版本，但必须获得用户明确授权。
- 使用 `java -version` 验证版本。
- 使用 `which java` 检查实际执行路径。

禁止：

- 使用 `/usr/bin/java`。
- 使用系统包管理器安装的 OpenJDK 作为项目运行环境。
- 使用系统自带 javac 编译项目。
- 未确认 SDKMAN! 环境就执行 Gradle、Maven 或其他 Java 构建命令。

如果项目要求的 Java 版本未安装：

1. 检查 `.sdkmanrc`。
2. 检查 `pom.xml`。
3. 检查 `build.gradle`、`build.gradle.kts`。
4. 检查 `gradle.properties` 和 Gradle Toolchain 配置。
5. 确认项目要求的 JDK 版本及发行版。
6. 使用 SDKMAN! 安装目标 JDK。
7. 使用 `sdk use java <version>` 切换版本。
8. 验证 `java` 和 `javac` 的路径及版本。
9. 确认使用 SDKMAN! 管理的 JDK 后再执行项目操作。

## 3. 项目版本识别规则

开始任何开发任务之前，必须优先识别项目声明的运行时版本。

按照以下文件和配置进行检查：

| 语言 | 优先检查的版本声明 |
|---|---|
| Go | `go.mod`、`go.work` |
| Python | `.python-version`、`pyproject.toml`、`Pipfile`、`runtime.txt` |
| Ruby | `.ruby-version`、`Gemfile` |
| Node.js | `.nvmrc`、`.node-version`、`package.json` |
| Java | `.sdkmanrc`、`pom.xml`、`build.gradle`、`build.gradle.kts` |

版本选择优先级：

1. 项目明确声明的运行时版本。
2. 项目构建工具或依赖明确要求的兼容版本。
3. 当前版本管理工具已经安装且符合项目要求的版本。
4. 如果不存在符合要求的已安装版本，则通过对应版本管理工具安装。

不得擅自升级、降级或修改项目要求的语言版本。

如果多个配置文件存在版本冲突：

1. 暂停运行项目命令。
2. 分析各配置文件的版本约束。
3. 判断实际构建工具和项目配置的要求。
4. 无法确定时向用户说明冲突，不得擅自选择一个版本。

## 4. 自动安装与版本切换

当发现当前环境不满足项目运行时要求时，必须主动处理。

执行流程：

1. 读取项目版本声明。
2. 检查对应版本管理工具是否安装。
3. 检查当前 Shell 是否正确加载版本管理工具。
4. 检查目标版本是否已经安装。
5. 如果没有安装，使用指定版本管理工具安装。
6. 安装完成后切换到目标版本。
7. 验证运行时路径和版本。
8. 确认运行时符合要求后继续执行任务。

对应操作：

```bash
# Go
gvm list
gvm install <version>
gvm use <version>

# Python
pyenv versions
pyenv install <version>
pyenv local <version>

# Ruby
rvm list
rvm install <version>
rvm use <version>

# Node.js
nvm list
nvm install <version>
nvm use <version>

# Java
sdk list java
sdk install java <version>
sdk use java <version>
```

注意：

- 版本号必须来自项目要求或经过确认的兼容版本。
- 不得猜测版本号。
- 不得使用系统包管理器作为安装失败时的替代方案。
- 不得擅自更改全局默认版本。
- 安装或编译运行时需要较长时间时，应合理处理并报告必要的进度和结果。

## 5. PATH 与 Shell 环境检查

版本管理工具必须正确集成到当前 Shell 环境。

开始执行项目命令前，应检查：

```bash
echo "$PATH"

which go
which python
which ruby
which node
which java
```

还应检查各工具自身的状态：

```bash
gvm current
pyenv version
rvm current
nvm current
sdk current java
```

实际执行路径必须符合对应工具的管理目录。

典型路径：

| 语言 | 预期路径特征 |
|---|---|
| Go | GVM 管理目录 |
| Python | `~/.pyenv/shims/` 或 pyenv 虚拟环境目录 |
| Ruby | `~/.rvm/` |
| Node.js | `~/.nvm/versions/node/` |
| Java | `~/.sdkman/candidates/java/` |

如果 PATH 顺序错误：

1. 检查 Shell 初始化文件。
2. 检查版本管理工具初始化脚本。
3. 检查是否存在覆盖 PATH 的配置。
4. 修复当前 Shell 环境。
5. 重新加载配置。
6. 再次验证运行时路径。

不得通过简单修改软链接或替换系统二进制文件来解决 PATH 问题。

## 6. 全局环境保护

用户已经拥有自己的语言版本管理环境。

Agent 必须保护已有环境，不得擅自修改用户的全局配置。

未经用户明确授权，禁止执行：

```bash
pyenv global <version>
nvm alias default <version>
rvm use <version> --default
gvm use <version> --default
sdk default java <version>
```

项目级版本优先使用项目配置文件固定。

推荐：

- Python：`.python-version`
- Node.js：`.nvmrc`
- Ruby：`.ruby-version`
- Java：`.sdkmanrc`
- Go：`go.mod` 和 `go.work` 中的版本声明

对于没有项目级版本文件的 Go 项目，应根据 `go.mod` 的要求通过 GVM 选择兼容版本，不得擅自改变用户的全局默认版本。

不得擅自：

- 删除已有版本。
- 卸载已有运行时。
- 清理版本管理工具目录。
- 修改用户全局默认版本。
- 覆盖用户 Shell 初始化文件。
- 删除已有虚拟环境。
- 执行可能影响其他项目的全局升级操作。

## 7. 依赖管理规范

所有语言依赖必须安装在对应版本管理工具所管理的运行时环境中。

Python：

- 使用当前 pyenv 管理的 Python。
- 项目依赖优先安装到项目虚拟环境。
- 使用 `python -m pip`，避免直接调用来源不明的 pip。

Node.js：

- 使用 NVM 管理的 Node.js。
- 项目依赖安装使用项目指定的 npm、pnpm 或 yarn。
- 不得使用系统 Node.js 安装全局依赖。

Ruby：

- 使用 RVM 管理的 Ruby。
- 使用对应 Ruby 环境的 gem 和 Bundler。
- 遵循项目 Gemfile 和 Gemfile.lock。

Go：

- 使用 GVM 管理的 Go。
- 遵循 go.mod 和 go.sum。
- 使用项目指定的 Go 版本执行构建和测试。

Java：

- 使用 SDKMAN! 管理的 JDK。
- 遵循 Maven、Gradle 和项目 Toolchain 配置。
- 确保构建工具实际使用的是 SDKMAN! 管理的 JDK。

## 8. 特殊情况处理

### 8.1 版本管理工具未安装

如果对应版本管理工具不存在：

1. 不得使用系统自带运行时代替。
2. 检查工具安装状态。
3. 向用户说明缺少的工具。
4. 在具备权限和条件时，按照用户现有环境的安装方式安装或修复。
5. 不得擅自覆盖现有配置。

### 8.2 版本安装失败

如果安装目标版本失败：

1. 检查下载、网络、编译依赖和系统兼容性。
2. 分析安装失败原因。
3. 尝试通过对应版本管理工具解决。
4. 必要时向用户说明问题并请求协助。
5. 不得退回系统自带运行时。

### 8.3 系统工具依赖

某些操作系统工具或系统维护程序可能依赖系统自带 Python、Ruby、Java 等运行时。

这些系统级工具属于操作系统维护范畴，不属于项目开发运行环境。

Agent 不得为了遵守本规范而破坏系统工具的正常运行。

但是，执行项目开发、构建、测试和运行命令时，仍然必须使用指定版本管理工具管理的运行时。

### 8.4 第三方工具自带运行时

如果第三方工具内置了独立运行时，必须先判断该运行时是否属于项目实际开发或构建环境。

不得因为第三方工具自带运行时而擅自将其作为项目运行时。

如果第三方工具强制依赖其内置运行时，应明确说明该特殊情况。

## 9. 任务执行前检查清单

每次开始项目开发任务前，必须完成以下检查：

- [ ] 确认项目使用哪些语言。
- [ ] 检查项目的运行时版本声明。
- [ ] 确认对应版本管理工具已正确加载。
- [ ] 确认目标运行时已经安装。
- [ ] 如果版本不满足要求，通过对应工具安装。
- [ ] 切换到符合项目要求的版本。
- [ ] 检查 PATH 和实际执行路径。
- [ ] 确认没有调用系统自带运行时。
- [ ] 确认依赖管理工具运行在正确的运行时环境中。
- [ ] 确认全局默认版本未被擅自修改。

## 10. 最终执行原则

本规范为强制性开发环境规范，优先级高于 Agent 自身的默认开发习惯。

必须始终遵循以下对应关系：

- Go → GVM
- Python → pyenv
- Ruby → RVM
- Node.js → NVM
- Java → SDKMAN!

**核心要求：如果版本不满足，先通过指定版本管理工具安装并切换；如果工具或环境异常，先修复工具或环境；绝不使用系统自带运行时作为替代方案。**