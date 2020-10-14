package tcpconverter

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/elemc/serial"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	readBuffer = 1024
)

// Converter - common converter interface
type Converter interface {
	Init() error
	Close() error
	Serve() error
	Stop()
}

type tcpConverter struct {
	*sync.RWMutex

	logger     *logrus.Logger
	netAddr    string
	tcpTimeout time.Duration
	config     *serial.Config

	dataToTCP   chan []byte
	dataToRS485 chan []byte

	tcpListener *net.TCPListener
	serialPort  serial.Port

	tcpConnections []*net.TCPConn

	stopFlag bool
}

// NewTCPConverter - create new converter pointer as interface
func NewTCPConverter(
	logger *logrus.Logger,
	tcpAddr string,
	tcpTimeout time.Duration,
	config *serial.Config,
) Converter {
	return &tcpConverter{
		RWMutex:     &sync.RWMutex{},
		logger:      logger,
		netAddr:     tcpAddr,
		tcpTimeout:  tcpTimeout,
		config:      config,
		dataToTCP:   make(chan []byte, 100),
		dataToRS485: make(chan []byte, 100),
	}
}

func (cnv *tcpConverter) Init() (err error) {
	if err = cnv.establishTCP(); err != nil {
		return
	}
	if err = cnv.establishSerial(); err != nil {
		return
	}
	return
}

// Close - close all connections
func (cnv *tcpConverter) Close() (err error) {
	cnv.Lock()
	defer cnv.Unlock()

	// TCP
	if cnv.tcpListener != nil {
		_ = cnv.tcpListener.Close()
	}
	for _, conn := range cnv.tcpConnections {
		_ = conn.Close()
	}

	// serial
	if cnv.serialPort != nil {
		if err = cnv.serialPort.Close(); err != nil {
			err = errors.Wrapf(err, "unable to close serial port: %s", cnv.config.Address)
			return
		}
	}

	return
}

// Serve - start converter
func (cnv *tcpConverter) Serve() (err error) {
	go cnv.toRS485Listen()
	go cnv.toTCPListener()

	go cnv.listenTCP()
	go cnv.listenSerial()

	return
}

// Stop - stop converter
func (cnv *tcpConverter) Stop() {
	cnv.Lock()
	cnv.stopFlag = true
	if cnv.dataToRS485 != nil {
		close(cnv.dataToRS485)
	}
	if cnv.dataToTCP != nil {
		close(cnv.dataToTCP)
	}
	cnv.Unlock()
}

func (cnv *tcpConverter) listenTCP() {
	cnv.logger.WithField("addr", cnv.netAddr).Info("Listen TCP start")
	for {
		if cnv.isStopped() {
			break
		}
		tcpConn, err := cnv.tcpListener.AcceptTCP()
		if err != nil {
			cnv.logger.
				WithField("addr", cnv.netAddr).
				WithError(err).Error("Unable to accept TCP connections")
			return
		}
		cnv.Lock()
		cnv.tcpConnections = append(cnv.tcpConnections, tcpConn)
		cnv.Unlock()
		go cnv.readerWorker(tcpConn)
	}
	cnv.logger.
		WithField("addr", cnv.netAddr).
		Info("Listen TCP stopped")
	return
}

func (cnv *tcpConverter) readerWorker(conn *net.TCPConn) {
	cnv.logger.
		WithField("addr", cnv.netAddr).
		Info("Read worker start")
	if err := conn.SetReadBuffer(readBuffer); err != nil {
		cnv.logger.
			WithError(err).
			WithField("addr", cnv.netAddr).
			Error("Unable to set read TCP connection buffer")
		return
	}
	for {
		if cnv.isStopped() {
			break
		}
		deadline := time.Now().Add(cnv.tcpTimeout)
		if err := conn.SetReadDeadline(deadline); err != nil {
			cnv.logger.
				WithError(err).
				WithField("addr", cnv.netAddr).
				WithField("deadline", deadline).
				Error("Unable to set read TCP deadline")
			continue
		}

		reader := bufio.NewReader(conn)
		data, err := reader.ReadBytes(0x0d)
		if err != nil {
			if netErr, ok := err.(net.Error); ok {
				if netErr.Timeout() {
					continue
				}
			}
			if err == io.EOF {
				break
			}
			cnv.logger.
				WithError(err).
				WithField("addr", cnv.netAddr).
				Error("Unable to read from TCP connection")
			continue
		}

		cnv.logger.
			WithField("addr", cnv.netAddr).
			WithField("hex", hex.EncodeToString(data)).
			WithField("data", bytes.NewBuffer(data).String()).
			Debug("Has new data from TCP")

		select {
		case cnv.dataToRS485 <- data:
			cnv.logger.
				WithField("data", bytes.NewBuffer(data).String()).
				WithField("hex", hex.EncodeToString(data)).
				WithField("addr", cnv.netAddr).
				Debug("Incoming data to RS-485")
		case <-time.After(cnv.tcpTimeout):
			cnv.logger.
				WithField("addr", cnv.netAddr).
				Warn("Channel data to RS-485 is busy")
			continue
		}
	}
	cnv.logger.WithField("addr", cnv.netAddr).Info("Read worker stopped")
}

func (cnv *tcpConverter) establishTCP() (err error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", cnv.netAddr)
	if err != nil {
		err = errors.Wrapf(err, "unable to resolve TCP/IP address: %s", cnv.netAddr)
		return
	}
	cnv.tcpListener, err = net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		err = errors.Wrapf(err, "unable to listen TCP address: %s", cnv.netAddr)
		return

	}
	return
}

func (cnv *tcpConverter) establishSerial() (err error) {
	if cnv.serialPort, err = serial.Open(cnv.config); err != nil {
		err = errors.Wrapf(err, "unable to open serial port: %s", cnv.config.Address)
	}
	return
}

func (cnv *tcpConverter) isStopped() bool {
	cnv.RLock()
	defer cnv.RUnlock()
	return cnv.stopFlag
}

func (cnv *tcpConverter) listenSerial() {
	cnv.logger.WithField("addr", cnv.config.Address).Info("Start listen serial")
	for {
		if cnv.isStopped() {
			break
		}

		reader := bufio.NewReader(cnv.serialPort)
		data, err := reader.ReadBytes(0x0d)
		if err != nil {
			if err != serial.ErrTimeout {
				cnv.logger.WithError(err).
					WithField("addr", cnv.config.Address).
					Error("Unable to read from serial port")
			}
			continue
		}

		cnv.logger.
			WithField("addr", cnv.config.Address).
			WithField("hex", hex.EncodeToString(data)).
			WithField("data", bytes.NewBuffer(data).String()).
			Debug("Has new data from serial")

		select {
		case cnv.dataToTCP <- data:
			cnv.logger.
				WithField("data", bytes.NewBuffer(data).String()).
				WithField("hex", hex.EncodeToString(data)).
				WithField("addr", cnv.config.Address).
				Debug("Incoming data to TCP")
		case <-time.After(cnv.tcpTimeout):
			cnv.logger.
				WithField("addr", cnv.config.Address).
				Warn("Channel data to TCP is busy")
			continue
		}

	}
	cnv.logger.WithField("addr", cnv.config.Address).Info("Stop listen serial")
}

func (cnv *tcpConverter) toRS485Listen() {
	cnv.logger.WithField("addr", cnv.config.Address).Info("Start data channel")
	for data := range cnv.dataToRS485 {
		n, err := cnv.serialPort.Write(data)
		if err != nil {
			cnv.logger.
				WithError(err).
				WithField("addr", cnv.config.Address).
				WithField("bytes", n).
				Error("Unable to write data to serial port")
		}
		cnv.logger.
			WithField("addr", cnv.config.Address).
			WithField("bytes", n).
			WithField("data", bytes.NewBuffer(data).String()).
			Debug("Write data to serial port")
	}
	cnv.logger.WithField("addr", cnv.config.Address).Info("Stop data channel")
}

func (cnv *tcpConverter) toTCPListener() {
	cnv.logger.WithField("addr", cnv.netAddr).Info("Start data channel")
	for data := range cnv.dataToTCP {
		for idx, conn := range cnv.tcpConnections {
			if conn == nil {
				continue
			}
			if _, err := conn.Write(data); err != nil {
				if errors.Is(err, syscall.EPIPE) {
					cnv.Lock()
					cnv.tcpConnections = append(cnv.tcpConnections[:idx], cnv.tcpConnections[idx+1:]...)
					cnv.Unlock()
					cnv.logger.
						WithField("addr", cnv.netAddr).
						WithField("remote", conn.RemoteAddr().String()).
						Info("TCP connection closed")
					continue
				}
				cnv.logger.
					WithError(err).
					WithField("addr", cnv.netAddr).
					WithField("remote", conn.RemoteAddr().String()).
					Error("Unable to write data to TCP connection")
			}
		}
	}
	cnv.logger.WithField("addr", cnv.netAddr).Info("Stop data channel")
}
