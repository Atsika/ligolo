package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/eiannone/keyboard"
	"github.com/hashicorp/yamux"
	"github.com/manifoldco/promptui"
	"github.com/sirupsen/logrus"
)

func main() {
	fmt.Print(`
██╗     ██╗ ██████╗  ██████╗ ██╗      ██████╗
██║     ██║██╔════╝ ██╔═══██╗██║     ██╔═══██╗
██║     ██║██║  ███╗██║   ██║██║     ██║   ██║
██║     ██║██║   ██║██║   ██║██║     ██║   ██║
███████╗██║╚██████╔╝╚██████╔╝███████╗╚██████╔╝
╚══════╝╚═╝ ╚═════╝  ╚═════╝ ╚══════╝ ╚═════╝
              Local Input - Go - Local Output

`)

	localServer := flag.String("localserver", "127.0.0.1:1080", "The local server address (your proxychains parameter)")
	relayServer := flag.String("relayserver", "0.0.0.0:5555", "The relay server listening address (the connect-back address)")
	certFile := flag.String("certfile", "certs/cert.pem", "The TLS server certificate")
	keyFile := flag.String("keyfile", "certs/key.pem", "The TLS server key")

	flag.Parse()

	relay := NewLigoloRelay(*localServer, *relayServer, *certFile, *keyFile)
	relay.Start()
}

// ConnPool contains yamux sessions and client's hostname
type ConnPool struct {
	Sessions []*yamux.Session
	Hostname []string
}

// LigoloRelay structure contains configuration, the current session and the ConnectionPool
type LigoloRelay struct {
	LocalServer    string
	RelayServer    string
	CertFile       string
	KeyFile        string
	ConnectionPool map[string]*yamux.Session
	Session        *yamux.Session
	Mutex          *sync.Mutex
	CurrentSession string
}

// NewLigoloRelay creates a new LigoloRelay struct
func NewLigoloRelay(localServer string, relayServer string, certFile string, keyFile string) *LigoloRelay {
	return &LigoloRelay{LocalServer: localServer, RelayServer: relayServer, CertFile: certFile, KeyFile: keyFile, ConnectionPool: make(map[string]*yamux.Session), Mutex: new(sync.Mutex)}
}

// Start listening for local and relay connections
func (ligolo *LigoloRelay) Start() {

	logrus.WithFields(logrus.Fields{"localserver": ligolo.LocalServer, "relayserver": ligolo.RelayServer}).Println("Ligolo server started.")
	go ligolo.startRelayHandler()
	go ligolo.startLocalHandler()

	if err := keyboard.Open(); err != nil {
		panic(err)
	}
	defer func() {
		_ = keyboard.Close()
	}()

	for {
		_, key, err := keyboard.GetKey()
		if err != nil {
			panic(err)
		}

		switch {
		case key == keyboard.KeyCtrlS:
			ligolo.SelectSession()

		case key == keyboard.KeyCtrlC:
			return

		default:
			logrus.Warn("\t[Ctrl + S] : Select session\n\t\t[Ctrl + C] : Quit")
		}

	}

}

// SelectSession allows to switch between available sessions
func (ligolo *LigoloRelay) SelectSession() error {

	if len(ligolo.ConnectionPool) == 0 {
		logrus.Warning("No client... Waiting...")
	}

	for len(ligolo.ConnectionPool) == 0 {
		time.Sleep(time.Second * 1)
	}

	items := make([]string, 0)
	for i := range ligolo.ConnectionPool {
		items = append(items, i)
	}

	if len(ligolo.ConnectionPool) == 1 {
		ligolo.Mutex.Lock()
		ligolo.CurrentSession = items[0]
		ligolo.Session = ligolo.ConnectionPool[items[0]]
		ligolo.Mutex.Unlock()
		logrus.Info("Session '", ligolo.CurrentSession, "' selected.")
		return nil
	}

	ligolo.Mutex.Lock()
	prompt := promptui.Select{
		Label: "Select client to proxy through ",
		Items: items,
	}
	var err error
	_, ligolo.CurrentSession, err = prompt.Run()
	if err != nil {
		fmt.Printf("Prompt failed %v\n", err)
		return err
	}
	ligolo.Session = ligolo.ConnectionPool[ligolo.CurrentSession]
	ligolo.Mutex.Unlock()
	logrus.Info("Session '", ligolo.CurrentSession, "' selected.")
	return nil
}

// RemoveSession deletes dead sessions
func (ligolo *LigoloRelay) RemoveSession() {
	logrus.Info("Removing session ", ligolo.CurrentSession)
	ligolo.Mutex.Lock()
	delete(ligolo.ConnectionPool, ligolo.CurrentSession)
	ligolo.Mutex.Unlock()
}

// Listen for Ligolo connections
func (ligolo *LigoloRelay) startRelayHandler() {
	cer, err := tls.LoadX509KeyPair(ligolo.CertFile, ligolo.KeyFile)
	if err != nil {
		logrus.Error("Could not load TLS certificate.")
		return
	}

	config := &tls.Config{Certificates: []tls.Certificate{cer}}
	listener, err := tls.Listen("tcp4", ligolo.RelayServer, config)
	if err != nil {
		logrus.Errorf("Could not bind to port : %v\n", err)

		return
	}
	defer listener.Close()
	for {
		c, err := listener.Accept()
		if err != nil {
			logrus.Errorf("Could not accept connection : %v\n", err)
			return
		}

		session, hostname, err := handleRelayConnection(c)
		if err != nil {
			logrus.Errorf("Could not start session : %v\n", err)
			continue
		}
		tmp := hostname
		ligolo.Mutex.Lock()
		for i := 1; ; i++ {
			if _, ok := ligolo.ConnectionPool[hostname]; ok {
				hostname = tmp + "-" + strconv.Itoa(i)
				continue
			}
			break
		}
		ligolo.ConnectionPool[hostname] = session
		logrus.Info("New session added to pool. Available clients : ", len(ligolo.ConnectionPool))
		ligolo.Mutex.Unlock()
	}

}

// Listen for local connections
func (ligolo *LigoloRelay) startLocalHandler() {
	listener, err := net.Listen("tcp4", ligolo.LocalServer)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer listener.Close()
	ligolo.SelectSession()
	go func() {
		for {
			<-ligolo.Session.CloseChan()
			logrus.WithFields(logrus.Fields{"remoteaddr": ligolo.Session.RemoteAddr()}).Println("Received session shutdown.")
			ligolo.RemoveSession()
			ligolo.SelectSession()
			logrus.WithFields(logrus.Fields{"remoteaddr": ligolo.Session.RemoteAddr()}).Println("New session acquired.")
		}
	}()
	logrus.Println("Session acquired. Starting relay.")
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}

		go ligolo.handleLocalConnection(conn)
	}
}

// Handle new local connections
func (ligolo *LigoloRelay) handleLocalConnection(conn net.Conn) {
	if ligolo.Session.IsClosed() {
		logrus.Warning("Closing connection because no session available !")
		conn.Close()
		if len(ligolo.ConnectionPool) > 0 {
			ligolo.RemoveSession()
			logrus.Warning("Session removed")
			ligolo.SelectSession()
			logrus.WithFields(logrus.Fields{"remoteaddr": ligolo.Session.RemoteAddr()}).Println("New session acquired.")
		}
		return
	}

	logrus.Println("New proxy connection. Establishing new session.")

	stream, err := ligolo.Session.Open()
	if err != nil {
		logrus.Errorf("Could not open session : %s\n", err)
		return
	}

	logrus.Println("Yamux session established.")

	go relay(conn, stream)
	go relay(stream, conn)

}

// Handle new ligolo connections
func handleRelayConnection(conn net.Conn) (*yamux.Session, string, error) {
	logrus.WithFields(logrus.Fields{"remoteaddr": conn.RemoteAddr().String()}).Info("New relay connection.\n")
	session, err := yamux.Server(conn, nil)
	if err != nil {
		return nil, "", err
	}
	ping, err := session.Ping()
	if err != nil {
		return nil, "", err
	}
	logrus.Printf("Session ping : %v\n", ping)

	init, err := session.Open()
	if err != nil {
		logrus.Error(err)
	}
	defer init.Close()

	hostname := make([]byte, 255)
	_, err = init.Read(hostname)
	if err != nil {
		logrus.Warning(err)
	}

	return session, string(hostname), nil
}

func relay(src net.Conn, dst net.Conn) {
	io.Copy(dst, src)
	dst.Close()
	src.Close()
	return
}
