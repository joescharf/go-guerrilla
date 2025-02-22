package guerrilla

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/log"
	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/mail/rfc5321"
	"github.com/flashmob/go-guerrilla/response"
)

const (
	CommandVerbMaxLength = 16
	CommandLineMaxLength = 1024
	// Number of allowed unrecognized commands before we terminate the connection
	MaxUnrecognizedCommands = 5
)

const (
	// server has just been created
	ServerStateNew = iota
	// Server has just been stopped
	ServerStateStopped
	// Server has been started and is running
	ServerStateRunning
	// Server could not start due to an error
	ServerStateStartError
)

// Server listens for SMTP clients on the port specified in its config
type server struct {
	configStore     atomic.Value // stores guerrilla.ServerConfig
	tlsConfigStore  atomic.Value
	timeout         atomic.Value // stores time.Duration
	listenInterface string
	clientPool      *Pool
	wg              sync.WaitGroup // for waiting to shutdown
	listener        net.Listener
	closedListener  chan bool
	hosts           allowedHosts // stores map[string]bool for faster lookup
	state           int
	// If log changed after a config reload, newLogStore stores the value here until it's safe to change it
	logStore     atomic.Value
	mainlogStore atomic.Value
	backendStore atomic.Value
	envelopePool *mail.Pool
}

type allowedHosts struct {
	table      map[string]bool // host lookup table
	wildcards  []string        // host wildcard list (* is used as a wildcard)
	sync.Mutex                 // guard access to the map
}

type command []byte

var (
	cmdHELO      command = []byte("HELO")
	cmdEHLO      command = []byte("EHLO")
	cmdHELP      command = []byte("HELP")
	cmdXCLIENT   command = []byte("XCLIENT")
	cmdMAIL      command = []byte("MAIL FROM:")
	cmdRCPT      command = []byte("RCPT TO:")
	cmdRSET      command = []byte("RSET")
	cmdVRFY      command = []byte("VRFY")
	cmdNOOP      command = []byte("NOOP")
	cmdQUIT      command = []byte("QUIT")
	cmdDATA      command = []byte("DATA")
	cmdSTARTTLS  command = []byte("STARTTLS")
	cmdAuth      command = []byte("AUTH LOGIN")
	cmdAuthPlain command = []byte("AUTH PLAIN")
)

func (c command) match(in []byte) bool {
	return bytes.Index(in, []byte(c)) == 0
}

// Creates and returns a new ready-to-run Server from a configuration
func newServer(sc *ServerConfig, b backends.Backend, l log.Logger) (*server, error) {
	server := &server{
		clientPool:      NewPool(sc.MaxClients),
		closedListener:  make(chan bool, 1),
		listenInterface: sc.ListenInterface,
		state:           ServerStateNew,
		envelopePool:    mail.NewPool(sc.MaxClients),
	}
	server.logStore.Store(l)
	server.backendStore.Store(b)
	logFile := sc.LogFile
	if logFile == "" {
		// none set, use the same log file as mainlog
		logFile = server.mainlog().GetLogDest()
	}
	// set level to same level as mainlog level
	mainlog, logOpenError := log.GetLogger(logFile, server.mainlog().GetLevel())
	server.mainlogStore.Store(mainlog)
	if logOpenError != nil {
		server.log().WithError(logOpenError).Errorf("Failed creating a logger for server [%s]", sc.ListenInterface)
	}

	server.setConfig(sc)
	server.setTimeout(sc.Timeout)
	if err := server.configureSSL(); err != nil {
		return server, err
	}
	return server, nil
}

func (s *server) configureSSL() error {
	sConfig := s.configStore.Load().(ServerConfig)
	if sConfig.TLS.AlwaysOn || sConfig.TLS.StartTLSOn {
		cert, err := tls.LoadX509KeyPair(sConfig.TLS.PublicKeyFile, sConfig.TLS.PrivateKeyFile)
		if err != nil {
			return fmt.Errorf("error while loading the certificate: %s", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ServerName:   sConfig.Hostname,
		}
		if len(sConfig.TLS.Protocols) > 0 {
			if min, ok := TLSProtocols[sConfig.TLS.Protocols[0]]; ok {
				tlsConfig.MinVersion = min
			}
		}
		if len(sConfig.TLS.Protocols) > 1 {
			if max, ok := TLSProtocols[sConfig.TLS.Protocols[1]]; ok {
				tlsConfig.MaxVersion = max
			}
		}
		if len(sConfig.TLS.Ciphers) > 0 {
			for _, val := range sConfig.TLS.Ciphers {
				if c, ok := TLSCiphers[val]; ok {
					tlsConfig.CipherSuites = append(tlsConfig.CipherSuites, c)
				}
			}
		}
		if len(sConfig.TLS.Curves) > 0 {
			for _, val := range sConfig.TLS.Curves {
				if c, ok := TLSCurves[val]; ok {
					tlsConfig.CurvePreferences = append(tlsConfig.CurvePreferences, c)
				}
			}
		}
		if len(sConfig.TLS.RootCAs) > 0 {
			caCert, err := ioutil.ReadFile(sConfig.TLS.RootCAs)
			if err != nil {
				s.log().WithError(err).Errorf("failed opening TLSRootCAs file [%s]", sConfig.TLS.RootCAs)
			} else {
				caCertPool := x509.NewCertPool()
				caCertPool.AppendCertsFromPEM(caCert)
				tlsConfig.RootCAs = caCertPool
			}

		}
		if len(sConfig.TLS.ClientAuthType) > 0 {
			if ca, ok := TLSClientAuthTypes[sConfig.TLS.ClientAuthType]; ok {
				tlsConfig.ClientAuth = ca
			}
		}
		tlsConfig.PreferServerCipherSuites = sConfig.TLS.PreferServerCipherSuites
		tlsConfig.Rand = rand.Reader
		s.tlsConfigStore.Store(tlsConfig)
	}
	return nil
}

// setBackend sets the backend to use for processing email envelopes
func (s *server) setBackend(b backends.Backend) {
	s.backendStore.Store(b)
}

// backend gets the backend used to process email envelopes
func (s *server) backend() backends.Backend {
	if b, ok := s.backendStore.Load().(backends.Backend); ok {
		return b
	}
	return nil
}

// Set the timeout for the server and all clients
func (s *server) setTimeout(seconds int) {
	duration := time.Duration(int64(seconds))
	s.clientPool.SetTimeout(duration)
	s.timeout.Store(duration)
}

// goroutine safe config store
func (s *server) setConfig(sc *ServerConfig) {
	s.configStore.Store(*sc)
}

// goroutine safe
func (s *server) isEnabled() bool {
	sc := s.configStore.Load().(ServerConfig)
	return sc.IsEnabled
}

// Set the allowed hosts for the server
func (s *server) setAllowedHosts(allowedHosts []string) {
	s.hosts.Lock()
	defer s.hosts.Unlock()
	s.hosts.table = make(map[string]bool, len(allowedHosts))
	s.hosts.wildcards = nil
	for _, h := range allowedHosts {
		if strings.Contains(h, "*") {
			s.hosts.wildcards = append(s.hosts.wildcards, strings.ToLower(h))
		} else {
			s.hosts.table[strings.ToLower(h)] = true
		}
	}
}

// Begin accepting SMTP clients. Will block unless there is an error or server.Shutdown() is called
func (s *server) Start(startWG *sync.WaitGroup) error {
	var clientID uint64
	clientID = 0

	listener, err := net.Listen("tcp", s.listenInterface)
	s.listener = listener
	if err != nil {
		startWG.Done() // don't wait for me
		s.state = ServerStateStartError
		return fmt.Errorf("[%s] Cannot listen on port: %s ", s.listenInterface, err.Error())
	}

	s.log().Infof("Listening on TCP %s", s.listenInterface)
	s.state = ServerStateRunning
	startWG.Done() // start successful, don't wait for me

	for {
		s.log().Debugf("[%s] Waiting for a new client. Next Client ID: %d", s.listenInterface, clientID+1)
		conn, err := listener.Accept()
		clientID++
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				s.log().Infof("Server [%s] has stopped accepting new clients", s.listenInterface)
				// the listener has been closed, wait for clients to exit
				s.log().Infof("shutting down pool [%s]", s.listenInterface)
				s.clientPool.ShutdownState()
				s.clientPool.ShutdownWait()
				s.state = ServerStateStopped
				s.closedListener <- true
				return nil
			}
			s.mainlog().WithError(err).Info("Temporary error accepting client")
			continue
		}
		go func(p Poolable, borrowErr error) {
			c := p.(*client)
			if borrowErr == nil {
				s.handleClient(c)
				s.envelopePool.Return(c.Envelope)
				s.clientPool.Return(c)
			} else {
				s.log().WithError(borrowErr).Info("couldn't borrow a new client")
				// we could not get a client, so close the connection.
				_ = conn.Close()

			}
			// intentionally placed Borrow in args so that it's called in the
			// same main goroutine.
		}(s.clientPool.Borrow(conn, clientID, s.log(), s.envelopePool))

	}
}

func (s *server) Shutdown() {
	if s.listener != nil {
		// This will cause Start function to return, by causing an error on listener.Accept
		_ = s.listener.Close()
		// wait for the listener to listener.Accept
		<-s.closedListener
		// At this point Start will exit and close down the pool
	} else {
		s.clientPool.ShutdownState()
		// listener already closed, wait for clients to exit
		s.clientPool.ShutdownWait()
		s.state = ServerStateStopped
	}
}

func (s *server) GetActiveClientsCount() int {
	return s.clientPool.GetActiveClientsCount()
}

// Verifies that the host is a valid recipient.
// host checking turned off if there is a single entry and it's a dot.
func (s *server) allowsHost(host string) bool {
	s.hosts.Lock()
	defer s.hosts.Unlock()
	// if hosts contains a single dot, further processing is skipped
	if len(s.hosts.table) == 1 {
		if _, ok := s.hosts.table["."]; ok {
			return true
		}
	}
	if _, ok := s.hosts.table[strings.ToLower(host)]; ok {
		return true
	}
	// check the wildcards
	for _, w := range s.hosts.wildcards {
		if matched, err := filepath.Match(w, strings.ToLower(host)); matched && err == nil {
			return true
		}
	}
	return false
}

// Verifies the client passed the auth if the auth required
func (s *server) isAuthentication(authRequired bool, loginStatus bool) bool {
	return !(authRequired) || loginStatus
}

const commandSuffix = "\r\n"

// Reads from the client until a \n terminator is encountered,
// or until a timeout occurs.
func (s *server) readCommand(client *client) ([]byte, error) {
	//var input string
	var err error
	var bs []byte
	// In command state, stop reading at line breaks
	bs, err = client.bufin.ReadSlice('\n')
	if err != nil {
		return bs, err
	} else if bytes.HasSuffix(bs, []byte(commandSuffix)) {
		return bs[:len(bs)-2], err
	}
	return bs[:len(bs)-1], err
}

// flushResponse a response to the client. Flushes the client.bufout buffer to the connection
func (s *server) flushResponse(client *client) error {
	if err := client.setTimeout(s.timeout.Load().(time.Duration)); err != nil {
		return err
	}
	return client.bufout.Flush()
}

func (s *server) isShuttingDown() bool {
	return s.clientPool.IsShuttingDown()
}

// Handles an entire client SMTP exchange
func (s *server) handleClient(client *client) {
	defer client.closeConn()
	sc := s.configStore.Load().(ServerConfig)
	s.log().Infof("Handle client [%s], id: %d", client.RemoteIP, client.ID)

	// Initial greeting
	greeting := fmt.Sprintf("220 %s SMTP Guerrilla(%s) #%d (%d) %s",
		sc.Hostname, Version, client.ID,
		s.clientPool.GetActiveClientsCount(), time.Now().Format(time.RFC3339))

	helo := fmt.Sprintf("250 %s Hello", sc.Hostname)
	// ehlo is a multi-line reply and need additional \r\n at the end
	ehlo := fmt.Sprintf("250-%s Hello\r\n", sc.Hostname)

	// Extended feature advertisements
	messageSize := fmt.Sprintf("250-SIZE %d\r\n", sc.MaxSize)
	advertiseAuth := "250-AUTH PLAIN LOGIN\r\n"
	pipelining := "250-PIPELINING\r\n"
	advertiseTLS := "250-STARTTLS\r\n"
	advertiseEnhancedStatusCodes := "250-ENHANCEDSTATUSCODES\r\n"
	// The last line doesn't need \r\n since string will be printed as a new line.
	// Also, Last line has no dash -
	help := "250 HELP"

	if sc.TLS.AlwaysOn {
		tlsConfig, ok := s.tlsConfigStore.Load().(*tls.Config)
		if !ok {
			s.mainlog().Error("Failed to load *tls.Config")
		} else if err := client.upgradeToTLS(tlsConfig); err == nil {
			advertiseTLS = ""
		} else {
			s.log().WithError(err).Warnf("[%s] Failed TLS handshake", client.RemoteIP)
			// server requires TLS, but can't handshake
			client.kill()
		}
	}
	if !sc.TLS.StartTLSOn {
		// STARTTLS turned off, don't advertise it
		advertiseTLS = ""
	}
	r := response.Canned
	authCmd := cmdAuthUsername
	loginInfo := &LoginInfo{
		status: false,
	}
	var initialResponse []byte
	for client.isAlive() {
		switch client.state {
		case ClientGreeting:
			client.sendResponse(greeting)
			client.state = ClientCmd
		case ClientCmd:
			client.bufin.setLimit(CommandLineMaxLength)
			input, err := s.readCommand(client)
			initialResponse = input
			s.log().Debugf("Client sent: %s", input)
			if err == io.EOF {
				s.log().WithError(err).Warnf("Client closed the connection: %s", client.RemoteIP)
				return
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.log().WithError(err).Warnf("Timeout: %s", client.RemoteIP)
				return
			} else if err == LineLimitExceeded {
				client.sendResponse(r.FailLineTooLong)
				client.kill()
				break
			} else if err != nil {
				s.log().WithError(err).Warnf("Read error: %s", client.RemoteIP)
				client.kill()
				break
			}
			if s.isShuttingDown() {
				client.state = ClientShutdown
				continue
			}

			cmdLen := len(input)
			if cmdLen > CommandVerbMaxLength {
				cmdLen = CommandVerbMaxLength
			}
			cmd := bytes.ToUpper(input[:cmdLen])
			switch {
			case cmdHELO.match(cmd):
				client.Helo = string(bytes.Trim(input[4:], " "))
				client.resetTransaction()
				client.sendResponse(helo)

			case cmdEHLO.match(cmd):
				client.Helo = string(bytes.Trim(input[4:], " "))
				client.resetTransaction()
				client.sendResponse(ehlo,
					messageSize,
					advertiseAuth,
					pipelining,
					advertiseTLS,
					advertiseEnhancedStatusCodes,
					help)

			case cmdHELP.match(cmd):
				quote := response.GetQuote()
				client.sendResponse("214-OK\r\n", quote)

			case sc.XClientOn && cmdXCLIENT.match(cmd):
				if toks := bytes.Split(input[8:], []byte{' '}); len(toks) > 0 {
					for i := range toks {
						if vals := bytes.Split(toks[i], []byte{'='}); len(vals) == 2 {
							if bytes.Equal(vals[1], []byte("[UNAVAILABLE]")) {
								// skip
								continue
							}
							if bytes.Equal(vals[0], []byte("ADDR")) {
								client.RemoteIP = string(vals[1])
							}
							if bytes.Equal(vals[0], []byte("HELO")) {
								client.Helo = string(vals[1])
							}
						}
					}
				}
				client.sendResponse(r.SuccessMailCmd)
			case cmdMAIL.match(cmd):
				if !s.isAuthentication(sc.AuthenticationRequired, loginInfo.status) {
					client.sendResponse(r.FailAuthRequired)
					break
				}
				if client.isInTransaction() {
					client.sendResponse(r.FailNestedMailCmd)
					break
				}
				client.MailFrom, err = client.parsePath(input[10:], client.parser.MailFrom)
				if err != nil {
					s.log().WithError(err).Error("MAIL parse error", "["+string(input[10:])+"]")
					client.sendResponse(err)
					break
				} else if client.parser.NullPath {
					// bounce has empty from address
					client.MailFrom = mail.Address{}
				}
				client.sendResponse(r.SuccessMailCmd)
			case cmdRCPT.match(cmd):
				if !s.isAuthentication(sc.AuthenticationRequired, loginInfo.status) {
					client.sendResponse(r.FailAuthRequired)
					break
				}
				if len(client.RcptTo) > rfc5321.LimitRecipients {
					client.sendResponse(r.ErrorTooManyRecipients)
					break
				}
				to, err := client.parsePath(input[8:], client.parser.RcptTo)
				if err != nil {
					s.log().WithError(err).Error("RCPT parse error", "["+string(input[8:])+"]")
					client.sendResponse(err.Error())
					break
				}
				if !s.allowsHost(to.Host) {
					client.sendResponse(r.ErrorRelayDenied, " ", to.Host)
				} else {
					client.PushRcpt(to)
					rcptError := s.backend().ValidateRcpt(client.Envelope)
					if rcptError != nil {
						client.PopRcpt()
						client.sendResponse(r.FailRcptCmd, " ", rcptError.Error())
					} else {
						client.sendResponse(r.SuccessRcptCmd)
					}
				}

			case cmdRSET.match(cmd):
				client.resetTransaction()
				client.sendResponse(r.SuccessResetCmd)

			case cmdVRFY.match(cmd):
				client.sendResponse(r.SuccessVerifyCmd)

			case cmdNOOP.match(cmd):
				client.sendResponse(r.SuccessNoopCmd)

			case cmdQUIT.match(cmd):
				client.sendResponse(r.SuccessQuitCmd)
				client.kill()

			case cmdDATA.match(cmd):
				if !s.isAuthentication(sc.AuthenticationRequired, loginInfo.status) {
					client.sendResponse(r.FailAuthRequired)
					break
				}
				if len(client.RcptTo) == 0 {
					client.sendResponse(r.FailNoRecipientsDataCmd)
					break
				}
				client.sendResponse(r.SuccessDataCmd)
				client.state = ClientData

			case cmdAuth.match(cmd):
				if loginInfo.status == true {
					client.sendResponse(r.FailNoIdentityChangesPermitted)
					break
				}
				// Status code and the base64 encoded "Username"
				client.sendResponse("334 VXNlcm5hbWU6")
				client.state = ClientAuth

			// AUTH PLAIN
			case cmdAuthPlain.match(cmd):
				if loginInfo.status == true {
					client.sendResponse(r.FailNoIdentityChangesPermitted)
					break
				}
				client.state = ClientAuthPlain

			case sc.TLS.StartTLSOn && cmdSTARTTLS.match(cmd):
				client.sendResponse(r.SuccessStartTLSCmd)
				client.state = ClientStartTLS
			default:
				client.errors++
				if client.errors >= MaxUnrecognizedCommands {
					client.sendResponse(r.FailMaxUnrecognizedCmd)
					client.kill()
				} else {
					client.sendResponse(r.FailUnrecognizedCmd)
				}
			}

		case ClientData:

			// intentionally placed the limit 1MB above so that reading does not return with an error
			// if the client goes a little over. Anything above will err
			client.bufin.setLimit(sc.MaxSize + 1024000) // This a hard limit.

			n, err := client.Data.ReadFrom(client.smtpReader.DotReader())
			if n > sc.MaxSize {
				err = fmt.Errorf("maximum DATA size exceeded (%d)", sc.MaxSize)
			}
			if err != nil {
				if err == LineLimitExceeded {
					client.sendResponse(r.FailReadLimitExceededDataCmd, " ", LineLimitExceeded.Error())
					client.kill()
				} else if err == MessageSizeExceeded {
					client.sendResponse(r.FailMessageSizeExceeded, " ", MessageSizeExceeded.Error())
					client.kill()
				} else {
					client.sendResponse(r.FailReadErrorDataCmd, " ", err.Error())
					client.kill()
				}
				s.log().WithError(err).Warn("Error reading data")
				client.resetTransaction()
				break
			}

			res := s.backend().Process(client.Envelope)
			if res.Code() < 300 {
				client.messagesSent++
			}
			client.sendResponse(res)
			client.state = ClientCmd
			if s.isShuttingDown() {
				client.state = ClientShutdown
			}
			client.resetTransaction()

		case ClientAuthPlain:
			// Based on https://github.com/emersion/go-smtp/blob/04984480e0a5c0f8646a33942ac02b999882a58c/conn.go

			// Parse initial response
			parts := strings.Fields(string(initialResponse))
			var ir []byte
			var err error
			if len(parts) > 1 {
				irTrim := string(bytes.Trim(initialResponse[10:], " "))
				ir, err = base64.StdEncoding.DecodeString(irTrim)
				if err != nil {
					break
				}
			}

			// Instantiate the sasl plain server handler
			sPlainSrv := sasl.NewPlainServer(func(identity, username, password string) error {
				loginInfo.username = username
				loginInfo.password = password

				// Validate the username and password from validate function
				userID, err := Authentication.Validate(loginInfo)
				s.log().Debugf("after Authentication.Validate(loginInfo): userID: %s, err: %s\n", userID, err)
				if err != nil {
					client.sendResponse(r.FailAuthNotAccepted)
					return err
				}

				loginInfo.status = true
				// In order to persist the authentication across multiple email submissions,
				// Must set the authID on the client session which is set on the envelope at
				// Reset and Borrow.
				client.setAuthID(userID)

				// client.Values["authID"] = userID
				if loginInfo.status {
					client.sendResponse(r.SuccessAuthentication)
				}
				// Reset the status of current command
				authCmd = cmdAuthUsername
				client.state = ClientCmd

				return nil
			})

			// Loop until we're done with the challenge/response cycle or error
			// TODO: Fix the error responses
			response := ir
			for {
				challenge, done, err := sPlainSrv.Next(response)
				if err != nil {
					client.sendResponse(r.FailAuthNotAccepted)
					break
				}
				// If we're done, we break out
				if done {
					break
				}
				// If the challenge > 0 we have to encode the string and write out to client
				encoded := ""
				if len(challenge) > 0 {
					encoded = base64.StdEncoding.EncodeToString(challenge)
				}
				client.sendResponse("334 " + encoded)

				// Read the next set of input from client
				input, err := s.readCommand(client)
				encoded = string(input)
				if err != nil {
					break // TODO: error handling
				}

				response, err = base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					client.sendResponse(r.FailAuthNotAccepted)
					break
				}

			}

		case ClientAuth:
			var err error
			switch {
			// Read the username from client
			case authCmd.match(cmdAuthUsername):
				var username string
				var bsUsername []byte
				username, err = client.authReader.ReadLine()
				if err != nil {
					break
				}
				bsUsername, err = base64.StdEncoding.DecodeString(username)
				loginInfo.username = string(bsUsername)
				if err != nil {
					break
				}
				// Status code and the base64 encoded Password
				client.sendResponse("334 UGFzc3dvcmQ6")
				authCmd = cmdAuthPassword
			// Read the password from client
			case authCmd.match(cmdAuthPassword):
				var password string
				var bsPassword []byte
				password, err = client.authReader.ReadLine()
				if err != nil {
					break
				}
				bsPassword, err = base64.StdEncoding.DecodeString(password)
				if err != nil {
					break
				}
				loginInfo.password = string(bsPassword)

				// Validate the username and password from validate function
				userID, err := Authentication.Validate(loginInfo)
				if err != nil {
					client.sendResponse(r.FailAuthNotAccepted)
					break
				}
				loginInfo.status = true

				// In order to persist the authentication across multiple email submissions,
				// Must set the authID on the client session which is set on the envelope at
				// Reset and Borrow.
				client.setAuthID(userID)

				if loginInfo.status {
					client.sendResponse(r.SuccessAuthentication)
				}
				// Reset the status of current command
				authCmd = cmdAuthUsername
				client.state = ClientCmd
			}

			if err == io.EOF {
				s.log().WithError(err).Warnf("Client closed the connection: %s", client.RemoteIP)
				return
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.log().WithError(err).Warnf("Timeout: %s", client.RemoteIP)
				return
			} else if err == LineLimitExceeded {
				client.sendResponse(r.FailLineTooLong)
				client.kill()
			} else if err != nil {
				s.log().WithError(err).Warnf("Read error: %s", client.RemoteIP)
				client.kill()
			}

			if s.isShuttingDown() {
				client.state = ClientShutdown
			}

		case ClientStartTLS:
			if !client.TLS && sc.TLS.StartTLSOn {
				tlsConfig, ok := s.tlsConfigStore.Load().(*tls.Config)
				if !ok {
					s.mainlog().Error("Failed to load *tls.Config")
				} else if err := client.upgradeToTLS(tlsConfig); err == nil {
					advertiseTLS = ""
					client.resetTransaction()
				} else {
					s.log().WithError(err).Warnf("[%s] Failed TLS handshake", client.RemoteIP)
					// Don't disconnect, let the client decide if it wants to continue
				}
			}
			// change to command state
			client.state = ClientCmd
		case ClientShutdown:
			// shutdown state
			client.sendResponse(r.ErrorShutdown)
			client.kill()
		}

		if client.bufErr != nil {
			s.log().WithError(client.bufErr).Debug("client could not buffer a response")
			return
		}
		// flush the response buffer
		if client.bufout.Buffered() > 0 {
			if s.log().IsDebug() {
				s.log().Debugf("Writing response to client: \n%s", client.response.String())
			}
			err := s.flushResponse(client)
			if err != nil {
				s.log().WithError(err).Debug("error writing response")
				return
			}
		}

	}
}

func (s *server) log() log.Logger {
	if l, ok := s.logStore.Load().(log.Logger); ok {
		return l
	}
	l, err := log.GetLogger(log.OutputStderr.String(), log.InfoLevel.String())
	if err == nil {
		s.logStore.Store(l)
	}
	return l
}

func (s *server) mainlog() log.Logger {
	if l, ok := s.mainlogStore.Load().(log.Logger); ok {
		return l
	}
	l, err := log.GetLogger(log.OutputStderr.String(), log.InfoLevel.String())
	if err == nil {
		s.mainlogStore.Store(l)
	}
	return l
}
