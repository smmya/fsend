# fsend 使用教程

在两台能互相连接的电脑之间发送文件或目录。支持 Linux、macOS、Windows；可发送多个任务，也可反向连接。

## 安装

### Linux / macOS

在终端执行：

```sh
curl -fsSL --proto '=https' https://raw.githubusercontent.com/smmya/fsend/main/install.sh | sh
```

默认安装到 `~/.local/bin/fsend`。如果提示找不到命令，执行下面一行，并将这一行加入你使用的 shell 配置文件：

```sh
export PATH="$HOME/.local/bin:$PATH"
```

指定版本或安装目录：

```sh
curl -fsSL --proto '=https' https://raw.githubusercontent.com/smmya/fsend/main/install.sh -o install-fsend.sh
FSEND_VERSION=v1.3.0 INSTALL_DIR="$HOME/.local/bin" sh install-fsend.sh
```

需要 `curl`、`tar` 及 `sha256sum` 或 `shasum`。支持 Linux amd64 / arm64 / armv7，macOS amd64 / arm64。

### Windows

在 PowerShell 执行：

```powershell
& ([scriptblock]::Create((Invoke-WebRequest -UseBasicParsing 'https://raw.githubusercontent.com/smmya/fsend/main/install.ps1').Content))
```

默认安装到 `%LOCALAPPDATA%\Programs\fsend`，并加入当前用户的 PATH。安装后重新打开终端。

指定版本和目录，或不修改 PATH：

```powershell
$script = (Invoke-WebRequest -UseBasicParsing 'https://raw.githubusercontent.com/smmya/fsend/main/install.ps1').Content
& ([scriptblock]::Create($script)) -Version v1.3.0 -InstallDir 'D:\Tools\fsend' -NoPath
```

支持 Windows amd64 / arm64。

### 手动下载

打开 [Releases](https://github.com/smmya/fsend/releases/latest)，选择与你的系统和处理器对应的压缩包，解压后运行 `fsend` 或 `fsend.exe`。安装脚本也可以在 Release 附件中下载。

检查安装结果：

```sh
fsend version
fsend --help
```

## 推荐：直接运行交互向导

双方都只需执行：

```sh
fsend
```

程序先让你选择 **发送文件 / 接收文件**，再选择连接方式：

- **输入对方给你的连接码**：对方已经给出 `fs://...` 时选这一项。
- **生成连接码，等待对方连接**：将生成的完整连接码发给对方。

最常用的操作：

1. 接收方选择 **2 接收文件**，连接方式直接回车，选择保存目录（回车使用默认目录），IP/域名也可回车自动检测。
2. 接收方将打印的连接码发给发送方。
3. 发送方选择 **1 发送文件**，连接方式直接回车，粘贴连接码。
4. 发送方逐行输入要发送的文件或目录路径，最后输入空行开始发送。

向导默认使用打包模式，不再额外询问模式；生成连接码后等待 10 分钟。路径带空格可以直接输入，也支持文件管理器拖入的成对引号路径。

如果接收方不能开放端口，让发送方选择“生成连接码”，接收方选择“输入连接码”即可完成反向连接。公网监听方仍需要开放端口并配置端口转发；IP/域名应填对方可访问的地址。

## 命令行兜底：接收方先启动

**1. 接收方启动：**

```sh
fsend receive -o ./received
```

程序会打印类似下面的混合密钥。将实际打印出的完整一行通过可信渠道发给发送方：

```text
fs://192.168.1.10:8977#XXXX-XXXX-XXXX-XXXX-XXXX-XXXX
```

默认等待 120 秒；超时后重新运行即可生成新密钥。

**2. 发送方启动：**

```sh
fsend send --mode tar
```

按提示粘贴混合密钥，再逐行输入要发送的文件或目录路径。路径带空格时可直接输入，也支持成对引号。输入完后再按一次回车开始传输。

```text
输入接收端混合密钥: 粘贴接收方给出的完整密钥
添加传输任务路径（直接回车结束）: /home/alice/photo.jpg
添加传输任务路径（直接回车结束）: /home/alice/Documents
添加传输任务路径（直接回车结束）:
```

Windows 输入示例：`C:\Users\Alice\Pictures\photo.jpg`。

如果希望每个文件分别确认，使用 `fsend send --mode serial`。直接使用 `send` 子命令且省略 `--mode`、`--path` 时，程序会让你选择模式。两个模式均支持空文件和空目录。请双方同时升级到 v1.3.0 或更新的兼容版本，以获得本次文件名修复。

**3. 等待两端显示完成。** 接收方在 `received` 目录中查看文件；遇到同名文件会自动添加序号，例如 `photo.1.jpg`。已有目录会合并，已有文件不会被覆盖。

### 不需要输入提示的命令

接收方启动监听后，发送方在一条命令中指定路径和实际连接码：

```sh
fsend send --path ./photo.jpg --path ./Documents 'fs://192.168.1.10:8977#这里替换为实际密钥'
```

`--path` 可以重复使用，支持文件和目录；路径参数放在连接码前面。使用 `--path` 时默认打包模式，需要逐文件模式时添加 `--mode serial`。

反向连接也可跳过所有提示：

```sh
# 发送方生成连接码并等待
fsend send -R --path ./Documents --ip your.example.com --wait 600
# 接收方使用发送方给出的实际连接码
fsend receive -R --key 'fs://your.example.com:8977#这里替换为实际密钥' -o ./received
```

连接码包含访问凭据，放在命令行中可能进入 shell 历史；日常使用推荐交互向导。

## 公网、指定地址与端口

同一局域网内，选用双方可达的局域网 IP。公网传输时，监听方需要开放防火墙端口，并在路由器上配置端口转发（如有）。指定对方可访问的公网 IP 或域名：

```sh
fsend receive -o ./received -p 8977 --ip your.example.com --wait 300
```

只绑定指定网卡：

```sh
fsend receive -b 192.168.1.10 --ip 192.168.1.10 -o ./received
```

本机测试：

```sh
fsend receive -b 127.0.0.1 --ip 127.0.0.1 -o ./received
```

自动显示的“出口 IP”通常是本地网卡地址，不能当作自动获取的公网 IP。该软件不会自动穿透 NAT，也没有中转服务。

## 反向连接：发送方先启动

接收方不方便开放端口时，让可开放端口的发送方监听。

发送方执行：

```sh
fsend send -R --ip your.example.com -p 8977 --wait 300 --mode tar
```

将打印的混合密钥发给接收方。接收方执行：

```sh
fsend receive -R -o ./received
```

接收方按提示粘贴密钥。连接成功后，发送方输入文件或目录路径，空行开始发送。接收方在反向模式下不用指定 `-p`、`-b`、`--ip` 或 `--wait`。

## 接收限额

默认每次最多接收 **100 GiB**、**100000 个文件和目录条目**。需要调整时：

```sh
fsend receive -o ./received --max-bytes 536870912000 --max-entries 200000
```

`--max-bytes 0` 表示不限总字节数，条目数限制仍然生效。这些参数同样适用于 `receive -R`。

## 注意事项与故障排查

- 混合密钥只发给预期的对方；任何拿到密钥的人都可能连接。每次启动都会生成新密钥，完成一次传输后进程退出。
- 使用普通用户运行，接收目录使用独立空目录；不需要管理员或 root 权限。
- 只传输常规文件和目录。软链接、设备文件等会被拒绝。不要在传输过程中修改源文件。
- 以点或空格结尾的文件及目录名：Linux/macOS 接收时保留原名；Windows 接收时将末尾点和空格分别保存为 `%2E`、`%20`，文件名里的 `%` 保存为 `%25`，避免不同原名相互混淆。例如 `folder. /file.` 保存为 `folder%2E%20/file%2E`。文件内容保持不变，两种模式均支持。
- 仍拒绝设备名、控制字符、`..` 路径段以及包含 `:<>"|?*` 的路径段。添加目录时会递归检查文件名，尽早提示不支持的名称。
- 当前版本不恢复时间戳、所有者、ACL、扩展属性及目录权限；普通文件仅保留基本权限位。
- 连接失败：检查 IP、监听端口、防火墙、端口转发，以及监听方是否已超时退出。
- 密钥校验失败：核对完整密钥，重新启动监听方生成新密钥。未经认证的探测连接会被拒绝，监听方会继续等待到 `--wait` 超时。
- 握手最多 30 秒；传输期间单帧读写最多等待 10 分钟。交互停顿过久也会断开，重新启动传输即可。
- 失败时返回非零退出码。串行模式已验证的文件保留；失败文件删除。打包模式校验失败时会清理本次创建的文件。新建的空目录可能保留；强制终止、断电或进程崩溃可能留下未完成文件。
- 没有断点续传。重试时已有文件保留并自动改名。

## 更新与卸载

更新：再次运行安装命令。安装脚本会从 GitHub Releases 下载最新正式版本并检查 SHA-256。

Linux / macOS 卸载默认安装：

```sh
rm "$HOME/.local/bin/fsend"
```

Windows：删除 `%LOCALAPPDATA%\Programs\fsend\fsend.exe`，并在“编辑账户的环境变量”中移除对应 PATH 项。自定义目录安装时，删除自定义目录下的程序。

## 从源码编译

Go 源码和回归测试统一位于 `src/` 目录，仓库根目录保留使用文档、安装脚本和项目配置。

安装 Go 1.26 或更新版本后：

```sh
git clone https://github.com/smmya/fsend.git
cd fsend
go build -trimpath -o fsend ./src
```

Windows 可改为 `go build -trimpath -o fsend.exe ./src`。主分支版本号首次发布时，GitHub Actions 会执行跨平台检查，自动创建对应标签并将安装包发布到 Releases。也支持推送 `vX.Y.Z` 标签触发；已有 Release 不会被覆盖。

开发说明见 [DESIGN.txt](DESIGN.txt)，审查记录见 [AUDIT.txt](AUDIT.txt)。
