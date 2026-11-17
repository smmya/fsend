package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// 版本号（version 子命令输出）
var appVersion = "1.4.2"

func main() {
	// 顶层 recover：任何 panic 都转为错误输出，绝不把栈信息抛给用户
	// Ctrl-C：干净退出，退出码 1（不打印栈）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		closeActivePicker()
		fmt.Fprintln(os.Stderr, "[fastsend] 已中断")
		os.Exit(1)
	}()

	code := 0
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[fastsend] 内部错误: %v\n", r)
				code = 1
			}
		}()
		code = run(os.Args[1:])
	}()
	os.Exit(code)
}

func run(args []string) int {
	return runWithInput(args, newInputScanner(os.Stdin))
}

func runWithInput(args []string, sc *bufio.Scanner) int {
	if len(args) == 0 {
		selected, err := interactiveArgs(sc)
		if err != nil {
			errf("读取选项失败: %v", err)
			return 2
		}
		if len(selected) == 0 {
			return 0
		}
		return runWithInput(selected, sc)
	}
	switch args[0] {
	case "receive":
		return runReceiveWithInput(args[1:], sc)
	case "send":
		return runSendWithInput(args[1:], sc)
	case "version":
		fmt.Printf("fsend %s (协议 v1, X25519+AES-256-GCM)\n", appVersion)
		return 0
	case "-h", "--help", "help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "[fastsend] 未知子命令: %s\n", args[0])
		printUsage()
		return 2
	}
}

// printUsage 用法输出到 stderr（退出码 2 场景）
func printUsage() {
	fmt.Fprint(os.Stderr, `用法:
  fsend                         交互向导（发送/接收、选择连接方向）
  fsend receive [-o 存储目录] [-p 监听端口] [-b 绑定地址] [--ip 对外IP] [--wait 秒]
  fsend send [混合密钥] [--mode tar|serial]
  fsend send --browse            打开多选文件浏览器
  fsend send --manual            手动输入路径
  fsend send --path 路径 [--path 其他路径] [--mode tar|serial] 混合密钥
  fsend version

反向模式（-R）: 由发送端监听并给出混合密钥，接收端拨号连入。
  fsend send -R [-p 端口] [-b 绑定地址] [--ip 对外IP] [--wait 秒] [--mode tar|serial]
  fsend receive -R [-o 存储目录] [--key 混合密钥]
适用于接收方位于 NAT 后、只有发送方可暴露端口的场景。
`)
}
