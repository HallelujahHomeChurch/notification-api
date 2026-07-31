package providers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"mime/quotedprintable"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMTPAcceptsDeliveryOverSTARTTLS(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{startTLS: true, auth: true})
	var output bytes.Buffer
	provider := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		Username:  "smtp-user",
		Password:  "smtp-password",
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
		Logger:    log.New(&output, "", 0),
	})
	payload := DeliveryPayload{
		Recipient: "person@example.test",
		Subject:   "Verify",
		Body:      "first line\n.leading dot\n驗證完成\n",
		MessageID: "<delivery-1@notification.alive.org.tw>",
	}

	receipt, err := provider.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if receipt.Provider != "smtp" || receipt.AcceptedAt.IsZero() {
		t.Fatalf("Send() receipt = %#v", receipt)
	}
	server.wait(t)
	commands := server.commands()
	if indexOfPrefix(commands, "STARTTLS") >= indexOfPrefix(commands, "AUTH ") {
		t.Fatalf("SMTP commands = %q, want STARTTLS before AUTH", commands)
	}
	for _, secret := range []string{payload.Recipient, payload.Subject, payload.Body, "smtp-user", "smtp-password"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log contains sensitive value %q: %q", secret, output.String())
		}
	}
	if !strings.Contains(output.String(), "event=notification_provider_success provider=smtp") {
		t.Fatalf("success log = %q", output.String())
	}
	assertSMTPData(t, server, payload)
}

func TestSMTPClassifiesLostFinalResponseAsAcceptanceUnknown(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{startTLS: true, dropAfterData: true})
	_, err := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
	}).Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorAcceptanceUnknown)
	if !server.accepted() {
		t.Fatal("SMTP server did not accept DATA before dropping the response")
	}
}

func TestSMTPReturnsCanceledContext(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{hangBeforeGreeting: true})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := NewSMTP(SMTPConfig{
		Addr:    server.addr,
		From:    "noreply@alive.org.tw",
		Timeout: 5 * time.Second,
	}).Send(ctx, validEmail())

	assertProviderError(t, err, ErrorTemporary)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Send() took %s after cancellation", elapsed)
	}
}

func TestSMTPBoundsConnectionWithTimeout(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{hangBeforeGreeting: true})

	start := time.Now()
	_, err := NewSMTP(SMTPConfig{
		Addr:    server.addr,
		From:    "noreply@alive.org.tw",
		Timeout: 50 * time.Millisecond,
	}).Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorTemporary)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Send() took %s, want bounded timeout", elapsed)
	}
}

func TestSMTPClassifiesTemporaryResponse(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{startTLS: true, recipientCode: 451})
	_, err := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
	}).Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorTemporary)
}

func TestSMTPClassifiesPermanentRecipientRejection(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{
		startTLS:        true,
		recipientCode:   550,
		responseMessage: "person@example.test rejected token=secret",
	})
	var output bytes.Buffer
	_, err := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
		Logger:    log.New(&output, "", 0),
	}).Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorPermanent)
	if !strings.Contains(output.String(), "event=notification_provider_failure provider=smtp") {
		t.Fatalf("failure log = %q", output.String())
	}
	for _, sensitive := range []string{"person@example.test", "token=secret"} {
		if strings.Contains(err.Error(), sensitive) || strings.Contains(output.String(), sensitive) {
			t.Fatalf("provider exposed sensitive SMTP response %q", sensitive)
		}
	}
}

func TestSMTPRejectsInvalidEndpointBeforeDial(t *testing.T) {
	_, err := NewSMTP(SMTPConfig{
		Addr:    "127.0.0.1:1",
		From:    "noreply@alive.org.tw",
		Timeout: time.Second,
	}).Send(context.Background(), DeliveryPayload{
		Recipient: "not-an-email",
		Subject:   "Verify",
		Body:      "body",
	})

	assertProviderError(t, err, ErrorInvalidEndpoint)
}

func TestValidateSMTPConfigRejectsStaticErrors(t *testing.T) {
	valid := SMTPConfig{
		Addr: "smtp.example.test:587",
		From: "noreply@alive.org.tw",
	}
	if err := ValidateSMTPConfig(valid); err != nil {
		t.Fatalf("ValidateSMTPConfig(valid) error = %v", err)
	}

	tests := []struct {
		name   string
		config SMTPConfig
	}{
		{
			name:   "address without port",
			config: SMTPConfig{Addr: "smtp.example.test", From: valid.From},
		},
		{
			name:   "non-numeric port",
			config: SMTPConfig{Addr: "smtp.example.test:not-a-port", From: valid.From},
		},
		{
			name:   "port out of range",
			config: SMTPConfig{Addr: "smtp.example.test:65536", From: valid.From},
		},
		{
			name:   "display name sender",
			config: SMTPConfig{Addr: valid.Addr, From: "HHC <noreply@alive.org.tw>"},
		},
		{
			name: "username without password",
			config: SMTPConfig{
				Addr: valid.Addr, From: valid.From, Username: "smtp-user",
			},
		},
		{
			name: "password without username",
			config: SMTPConfig{
				Addr: valid.Addr, From: valid.From, Password: "smtp-password",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSMTPConfig(test.config); err == nil {
				t.Fatal("ValidateSMTPConfig() error = nil")
			}
		})
	}
}

func TestSMTPNeverAuthenticatesWithoutSTARTTLS(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{})
	_, err := NewSMTP(SMTPConfig{
		Addr:     server.addr,
		Username: "smtp-user",
		Password: "smtp-password",
		From:     "noreply@alive.org.tw",
		Timeout:  time.Second,
	}).Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorPermanent)
	if commands := strings.Join(server.commands(), "\n"); strings.Contains(commands, "AUTH ") {
		t.Fatalf("SMTP commands contain AUTH before TLS: %q", commands)
	}
}

func TestSMTPAcceptsLoginAuthentication(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{startTLS: true, authMechanisms: "LOGIN"})
	provider := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		Username:  "smtp-user",
		Password:  "smtp-password",
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
	})

	if _, err := provider.Send(context.Background(), validEmail()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	server.wait(t)
	if indexOfPrefix(server.commands(), "AUTH LOGIN") == len(server.commands()) {
		t.Fatalf("SMTP commands = %q, want AUTH LOGIN", server.commands())
	}
}

func TestSMTPRequiresSupportedAuthenticationCapability(t *testing.T) {
	for _, mechanisms := range []string{"", "CRAM-MD5"} {
		t.Run("mechanisms="+mechanisms, func(t *testing.T) {
			server := newSMTPServer(t, smtpServerOptions{startTLS: true, authMechanisms: mechanisms})
			_, err := NewSMTP(SMTPConfig{
				Addr:      server.addr,
				Username:  "smtp-user",
				Password:  "smtp-password",
				From:      "noreply@alive.org.tw",
				Timeout:   time.Second,
				TLSConfig: server.clientTLS,
			}).Send(context.Background(), validEmail())

			assertProviderError(t, err, ErrorPermanent)
			if commands := strings.Join(server.commands(), "\n"); strings.Contains(commands, "AUTH ") {
				t.Fatalf("SMTP commands contain unsupported AUTH attempt: %q", commands)
			}
		})
	}
}

func TestSMTPAbortsDATAWithoutFinalizingAfterWriteFailure(t *testing.T) {
	server := newSMTPServer(t, smtpServerOptions{startTLS: true})
	provider := NewSMTP(SMTPConfig{
		Addr:      server.addr,
		From:      "noreply@alive.org.tw",
		Timeout:   time.Second,
		TLSConfig: server.clientTLS,
	})
	provider.writeMessage = func(writer io.Writer, _ string, _ DeliveryPayload) error {
		_, _ = io.WriteString(writer, "partial message")
		return io.ErrUnexpectedEOF
	}

	receipt, err := provider.Send(context.Background(), validEmail())

	assertProviderError(t, err, ErrorTemporary)
	if receipt != (ProviderReceipt{}) {
		t.Fatalf("Send() receipt = %#v, want empty receipt", receipt)
	}
	server.wait(t)
	if server.accepted() || server.terminated() {
		t.Fatalf("partial DATA was finalized: accepted=%t terminated=%t data=%q",
			server.accepted(), server.terminated(), server.data())
	}
}

func TestProviderErrorRetryBoundaries(t *testing.T) {
	tests := []struct {
		kind      ErrorKind
		retryable bool
	}{
		{ErrorTemporary, true},
		{ErrorRateLimited, true},
		{ErrorAcceptanceUnknown, true},
		{ErrorPermanent, false},
		{ErrorInvalidEndpoint, false},
		{ErrorSuppressed, false},
	}
	for _, test := range tests {
		err := &ProviderError{Kind: test.kind}
		if got := err.Retryable(); got != test.retryable {
			t.Fatalf("ProviderError{%q}.Retryable() = %t, want %t", test.kind, got, test.retryable)
		}
	}
}

func validEmail() DeliveryPayload {
	return DeliveryPayload{
		Recipient: "person@example.test",
		Subject:   "Verify",
		Body:      "Use the verification link.",
		MessageID: "<delivery-1@notification.alive.org.tw>",
	}
}

func assertProviderError(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %T %v, want *ProviderError", err, err)
	}
	if providerErr.Kind != kind {
		t.Fatalf("ProviderError.Kind = %q, want %q", providerErr.Kind, kind)
	}
}

type smtpServerOptions struct {
	startTLS           bool
	auth               bool
	authMechanisms     string
	recipientCode      int
	responseMessage    string
	hangBeforeGreeting bool
	dropAfterData      bool
}

type smtpServer struct {
	addr            string
	clientTLS       *tlsConfig
	listener        net.Listener
	mu              sync.Mutex
	seen            []string
	rawData         []byte
	finalized       bool
	acceptedMessage bool
	done            chan struct{}
}

type tlsConfig = tls.Config

func newSMTPServer(t *testing.T, options smtpServerOptions) *smtpServer {
	t.Helper()
	serverTLS, clientTLS := testTLSConfigs(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &smtpServer{
		addr:      listener.Addr().String(),
		clientTLS: clientTLS,
		listener:  listener,
		done:      make(chan struct{}),
	}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve(options, serverTLS)
	return server
}

func (s *smtpServer) serve(options smtpServerOptions, serverTLS *tlsConfig) {
	defer close(s.done)
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if options.hangBeforeGreeting {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeSMTP(writer, 220, "smtp.test ready")
	tlsActive := false
	loginStep := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		s.record(line)
		command := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(command, "EHLO "):
			if options.startTLS && !tlsActive {
				_, _ = writer.WriteString("250-smtp.test\r\n250 STARTTLS\r\n")
			} else if tlsActive && (options.auth || options.authMechanisms != "") {
				mechanisms := options.authMechanisms
				if mechanisms == "" {
					mechanisms = "PLAIN"
				}
				_, _ = writer.WriteString("250-smtp.test\r\n250 AUTH " + mechanisms + "\r\n")
			} else {
				writeSMTP(writer, 250, "smtp.test")
				continue
			}
			_ = writer.Flush()
		case command == "STARTTLS":
			writeSMTP(writer, 220, "ready for TLS")
			tlsConn := tls.Server(conn, serverTLS)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
			writer = bufio.NewWriter(conn)
			tlsActive = true
		case command == "AUTH LOGIN":
			if !tlsActive {
				writeSMTP(writer, 538, "encryption required")
			} else {
				loginStep = 1
				writeSMTP(writer, 334, base64.StdEncoding.EncodeToString([]byte("Username:")))
			}
		case loginStep == 1:
			username, err := base64.StdEncoding.DecodeString(line)
			if err != nil || string(username) != "smtp-user" {
				writeSMTP(writer, 535, "authentication failed")
			} else {
				loginStep = 2
				writeSMTP(writer, 334, base64.StdEncoding.EncodeToString([]byte("Password:")))
			}
		case loginStep == 2:
			password, err := base64.StdEncoding.DecodeString(line)
			if err != nil || string(password) != "smtp-password" {
				writeSMTP(writer, 535, "authentication failed")
			} else {
				loginStep = 0
				writeSMTP(writer, 235, "authenticated")
			}
		case strings.HasPrefix(command, "AUTH "):
			if !tlsActive {
				writeSMTP(writer, 538, "encryption required")
			} else {
				writeSMTP(writer, 235, "authenticated")
			}
		case strings.HasPrefix(command, "MAIL FROM:"):
			writeSMTP(writer, 250, "sender accepted")
		case strings.HasPrefix(command, "RCPT TO:"):
			code := options.recipientCode
			if code == 0 {
				code = 250
			}
			message := options.responseMessage
			if message == "" {
				message = "recipient response"
			}
			writeSMTP(writer, code, message)
		case command == "DATA":
			writeSMTP(writer, 354, "send data")
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" {
					s.markFinalized()
					break
				}
				s.recordData(dataLine)
			}
			if options.dropAfterData {
				s.markAccepted()
				return
			}
			writeSMTP(writer, 250, "queued")
			s.markAccepted()
		case command == "QUIT":
			writeSMTP(writer, 221, "bye")
			return
		default:
			writeSMTP(writer, 500, "unsupported")
		}
	}
}

func (s *smtpServer) record(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, command)
}

func (s *smtpServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *smtpServer) recordData(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawData = append(s.rawData, value...)
}

func (s *smtpServer) markFinalized() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = true
}

func (s *smtpServer) markAccepted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acceptedMessage = true
}

func (s *smtpServer) data() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.rawData...)
}

func (s *smtpServer) terminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalized
}

func (s *smtpServer) accepted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptedMessage
}

func (s *smtpServer) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("SMTP server did not stop")
	}
}

func writeSMTP(writer *bufio.Writer, code int, message string) {
	_, _ = writer.WriteString(strconv.Itoa(code) + " " + message + "\r\n")
	_ = writer.Flush()
}

func indexOfPrefix(values []string, prefix string) int {
	for index, value := range values {
		if strings.HasPrefix(strings.ToUpper(value), prefix) {
			return index
		}
	}
	return len(values)
}

func assertSMTPData(t *testing.T, server *smtpServer, payload DeliveryPayload) {
	t.Helper()
	data := server.data()
	wantHeaders := strings.Join([]string{
		"From: noreply@alive.org.tw",
		"To: person@example.test",
		"Subject: Verify",
		"Message-ID: " + payload.MessageID,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"",
	}, "\r\n")
	if !bytes.HasPrefix(data, []byte(wantHeaders)) {
		t.Fatalf("DATA headers = %q, want prefix %q", data, wantHeaders)
	}
	if hasBareLF(data) {
		t.Fatalf("DATA contains bare LF: %q", data)
	}
	if !bytes.Contains(data, []byte("\r\n..leading dot\r\n")) {
		t.Fatalf("DATA does not dot-stuff leading dot: %q", data)
	}
	if !server.terminated() || !server.accepted() {
		t.Fatalf("DATA termination = %t accepted = %t", server.terminated(), server.accepted())
	}

	separator := bytes.Index(data, []byte("\r\n\r\n"))
	encodedBody := append([]byte(nil), data[separator+4:]...)
	encodedBody = bytes.ReplaceAll(encodedBody, []byte("\r\n.."), []byte("\r\n."))
	decodedBody, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(encodedBody)))
	if err != nil {
		t.Fatalf("decode quoted-printable DATA: %v", err)
	}
	wantBody := strings.ReplaceAll(payload.Body, "\n", "\r\n")
	if string(decodedBody) != wantBody {
		t.Fatalf("decoded DATA body = %q, want %q", decodedBody, wantBody)
	}
}

func hasBareLF(value []byte) bool {
	for index, current := range value {
		if current == '\n' && (index == 0 || value[index-1] != '\r') {
			return true
		}
	}
	return false
}

func testTLSConfigs(t *testing.T) (*tlsConfig, *tlsConfig) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	parsedCertificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsedCertificate)
	return &tlsConfig{Certificates: []tls.Certificate{certificate}}, &tlsConfig{RootCAs: roots}
}
