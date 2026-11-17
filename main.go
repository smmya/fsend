package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// 版本号（version 子命令输出）
var appVersion = "1.2.0"

func main() {
	// 顶层 recover：任何 panic 都转为错误输出，绝不把栈信息抛给用户
	// Ctrl-C：干净退出，退出码 1（不打印栈）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
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

	if len(args) == 0 {
		printUsage()
		return 2
	}
	switch args[0] {
	case "receive":
		return runReceive(args[1:])
	case "send":
		return runSend(args[1:])
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
  fsend receive [-o 存储目录] [-p 监听端口] [-b 绑定地址] [--ip 对外IP] [--wait 秒]
  fsend send [混合密钥] [--mode tar|serial]
  fsend version

反向模式（-R）: 由发送端监听并给出混合密钥，接收端拨号连入。
  fsend send -R [-p 端口] [-b 绑定地址] [--ip 对外IP] [--wait 秒] [--mode tar|serial]
  fsend receive -R [-o 存储目录]
适用于接收方位于 NAT 后、只有发送方可暴露端口的场景。
`)
}
