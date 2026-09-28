//go:build linux

package main

import (
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// readSecretNoEcho 从终端读入一行，读的时候关闭回显。
//
// 关回显而不是「读完再擦掉」：密钥在输入过程中就不该出现在屏幕上，
// 终端的回滚缓冲与录屏都会把它留下。终端属性在读完时无论成败都恢复，
// 留着回显关闭的状态会让下一条命令行看不见自己敲的字。
func readSecretNoEcho(file *os.File) (string, error) {
	fd := int(file.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", fmt.Errorf("读取终端属性失败：%v", err)
	}
	hidden := *original
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
		return "", fmt.Errorf("关闭终端回显失败：%v", err)
	}
	restore := func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, original)
	}
	defer restore()

	// 读到一半被 Ctrl-C 打断时也要恢复：默认的 SIGINT 动作是直接终止进程，
	// defer 不会执行，终端会留在关闭回显的状态，之后看不见自己敲的字。
	// 这里先恢复属性，再撤掉订阅并把信号交回默认处理，退出码仍按惯例由 shell 给出。
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer func() {
		signal.Stop(sigint)
		close(sigint)
	}()
	go func() {
		sig, ok := <-sigint
		if !ok {
			return
		}
		restore()
		signal.Stop(sigint)
		if process, err := os.FindProcess(os.Getpid()); err == nil {
			_ = process.Signal(sig)
		}
	}()

	line := make([]byte, 0, 128)
	buf := make([]byte, 1)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			switch buf[0] {
			case '\n':
				return string(line), nil
			case '\r':
			default:
				line = append(line, buf[0])
			}
		}
		if err != nil {
			if len(line) > 0 {
				return string(line), nil
			}
			return "", fmt.Errorf("读取终端输入失败：%v", err)
		}
	}
}
