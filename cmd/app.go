package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/elemc/tcpconverter"
	"github.com/sirupsen/logrus"

	"github.com/elemc/serial"
)

func main() {
	var (
		tcpAddr    string
		tcpTimeout time.Duration
	)
	config := &serial.Config{RS485: serial.RS485Config{Enabled: true}}

	flag.StringVar(&tcpAddr, "tcp-addr", "localhost:4001", "TCP address and port")
	flag.DurationVar(&tcpTimeout, "tcp-timeout", time.Millisecond*100, "TCP timeout")

	flag.StringVar(&config.Address, "serial-addr", "/dev/ttyS0", "serial address")
	flag.IntVar(&config.BaudRate, "baud-rate", 9600, "baud rate ")
	flag.IntVar(&config.BaudRate, "data-bits", 8, "data bits (5, 6, 7, 8)")
	flag.IntVar(&config.BaudRate, "stop-bit", 1, "stop bit (1 or 2)")
	flag.StringVar(&config.Address, "parity", "N", "parity")
	flag.DurationVar(&config.Timeout, "serial-timeout", time.Millisecond*100, "TCP timeout")
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:    true,
		TimestampFormat:  time.RFC3339Nano,
		DisableTimestamp: false,
	},
	)

	converter := tcpconverter.NewTCPConverter(
		logger,
		tcpAddr,
		tcpTimeout,
		config,
	)
	if err := converter.Init(); err != nil {
		logger.Fatal(err)
	}
	if err := converter.Serve(); err != nil {
		logger.Fatal(err)
	}

	signalChannel := make(chan os.Signal)
	signal.Notify(signalChannel, syscall.SIGTERM)
	signal.Notify(signalChannel, syscall.SIGINT)
	signal.Notify(signalChannel, syscall.SIGKILL)
	<-signalChannel

	converter.Stop()
	_ = converter.Close()
}
