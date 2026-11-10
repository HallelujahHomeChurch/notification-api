package providers

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
)

func validateEmailPayload(payload DeliveryPayload) error {
	if !plainAddress(payload.Recipient) {
		return errors.New("invalid email address")
	}
	if strings.ContainsAny(payload.Subject, "\r\n") {
		return errors.New("invalid subject")
	}
	if strings.ContainsAny(payload.ListUnsubscribe, "\r\n") {
		return errors.New("invalid unsubscribe header")
	}
	if !validMessageID(payload.MessageID) {
		return errors.New("invalid message ID")
	}
	return nil
}

func plainAddress(value string) bool {
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value
}

func validMessageID(value string) bool {
	return len(value) >= 3 &&
		len(value) <= 998 &&
		strings.HasPrefix(value, "<") &&
		strings.HasSuffix(value, ">") &&
		!strings.ContainsAny(value, "\r\n \t") &&
		plainAddress(value[1:len(value)-1])
}

func writeEmailMIME(writer io.Writer, from, fromName string, payload DeliveryPayload) error {
	fromHeader := from
	if fromName != "" {
		fromHeader = (&mail.Address{Name: fromName, Address: from}).String()
	}
	if payload.HTMLBody != "" {
		return writeMultipartMessage(writer, fromHeader, payload)
	}
	headers := strings.Join(messageHeaders(fromHeader, payload, "Content-Type: text/plain; charset=UTF-8", "Content-Transfer-Encoding: quoted-printable"), "\r\n")
	if _, err := io.WriteString(writer, headers+"\r\n"); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}
	body := quotedprintable.NewWriter(writer)
	if _, err := io.WriteString(body, payload.Body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := body.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return nil
}

func writeMultipartMessage(writer io.Writer, from string, payload DeliveryPayload) error {
	multipartWriter := multipart.NewWriter(writer)
	headers := strings.Join(messageHeaders(from, payload, "Content-Type: multipart/alternative; boundary="+strconv.Quote(multipartWriter.Boundary())), "\r\n")
	if _, err := io.WriteString(writer, headers+"\r\n"); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}
	for _, part := range []struct {
		contentType string
		body        string
	}{
		{"text/plain; charset=UTF-8", payload.Body},
		{"text/html; charset=UTF-8", payload.HTMLBody},
	} {
		partWriter, err := multipartWriter.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return fmt.Errorf("create MIME part: %w", err)
		}
		bodyWriter := quotedprintable.NewWriter(partWriter)
		if _, err := io.WriteString(bodyWriter, part.body); err != nil {
			return fmt.Errorf("write MIME part: %w", err)
		}
		if err := bodyWriter.Close(); err != nil {
			return fmt.Errorf("close MIME part: %w", err)
		}
	}
	if err := multipartWriter.Close(); err != nil {
		return fmt.Errorf("close MIME message: %w", err)
	}
	return nil
}

func messageHeaders(from string, payload DeliveryPayload, extra ...string) []string {
	headers := []string{"From: " + from, "To: " + payload.Recipient, "Subject: " + mime.QEncoding.Encode("UTF-8", payload.Subject), "Message-ID: " + payload.MessageID, "MIME-Version: 1.0"}
	if payload.ListUnsubscribe != "" {
		headers = append(headers, "List-Unsubscribe: "+payload.ListUnsubscribe)
		if payload.OneClickUnsubscribe {
			headers = append(headers, "List-Unsubscribe-Post: List-Unsubscribe=One-Click")
		}
	}
	return append(headers, append(extra, "")...)
}
