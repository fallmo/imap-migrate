package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/mattn/go-tty"
)

func main() {
	reader := bufio.NewReader(os.Stdin)
	srcServer := "imap.gmail.com:993"
	dstServer := "smtp.heritage.africa:993"
	dstPass := "Accel@2025"
	mboxPattern := "*"
	batchSize := 200

	fmt.Println("Welcome fellow change maker. This program will copy your mails from gmail onto heritage. Before we begin, we will need some information.")
	fmt.Print("Your email: ")
	srcUser, _ := reader.ReadString('\n')
	srcUser = strings.TrimSpace(srcUser)

	srcPass := readPasswordMasked("Your google app password: ")

	fmt.Println("Connecting to google...")
	src, err := dialIMAP(srcServer, srcUser, srcPass, true)
	check(err)
	defer src.Logout()

	fmt.Println("Connecting to heritage...")
	dst, err := dialIMAP(dstServer, srcUser, dstPass, true)
	check(err)
	defer dst.Logout()

	fmt.Print("\nConnections Succesful. Press enter to begin (this may take a while)")
	_, _ = reader.ReadString('\n')

	fmt.Println("Listing mailboxes...")
	mboxes := listMailboxes(src, mboxPattern)

	for _, m := range mboxes {
		fmt.Printf("Syncing mailbox: %s\n", m)
		moved, err := syncMailbox(src, dst, m, batchSize)
		if err != nil {
			log.Printf("Error syncing %s: %v", m, err)
			continue
		}
		fmt.Printf("Done %s: moved %d messages\n", m, moved)
	}

	fmt.Println("Sync complete.")
}

// ---------------- IMAP helper functions ----------------

func dialIMAP(addr, user, pass string, useSSL bool) (*client.Client, error) {
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var c *client.Client
	var err error
	if useSSL {
		c, err = client.DialWithDialerTLS(dialer, addr, &tls.Config{ServerName: hostFromAddr(addr)})
	} else {
		c, err = client.DialWithDialer(dialer, addr)
	}
	if err != nil {
		return nil, err
	}
	if err = c.Login(user, pass); err != nil {
		c.Logout()
		return nil, err
	}
	return c, nil
}

func hostFromAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func listMailboxes(c *client.Client, pattern string) []string {
	mboxes := []string{}
	ch := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", pattern, ch)
	}()
	for m := range ch {
		skip := false
		for _, attr := range m.Attributes {
			if strings.EqualFold(attr, `\Noselect`) {
				skip = true
				break
			}
		}
		if !skip {
			mboxes = append(mboxes, m.Name)
		}
	}
	if err := <-done; err != nil {
		log.Fatal(err)
	}
	return mboxes
}

func syncMailbox(src, dst *client.Client, name string, batchSize int) (int, error) {
	mbox, err := src.Select(name, true)
	if err != nil {
		return 0, err
	}
	if mbox.Messages == 0 {
		fmt.Printf("[%s] Mailbox is empty\n", name)
		return 0, nil
	}
	if err := ensureMailbox(dst, name); err != nil {
		return 0, err
	}
	if _, err := dst.Select(name, false); err != nil {
		return 0, err
	}

	total := int(mbox.Messages)
	fmt.Printf("[%s] Mails total count: %d\n", name, total)

	moved := 0
	var from uint32 = 1
	for from <= mbox.Messages {
		to := from + uint32(batchSize) - 1
		if to > mbox.Messages {
			to = mbox.Messages
		}
		if err := copyBatch(src, dst, name, from, to, total, &moved); err != nil {
			return moved, err
		}
		from = to + 1
	}
	return moved, nil
}

func ensureMailbox(c *client.Client, name string) error {
	_, err := c.Select(name, true)
	if err == nil {
		return nil
	}
	return c.Create(name)
}

func copyBatch(src, dst *client.Client, mailbox string, from, to uint32, total int, moved *int) error {
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, to)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, section.FetchItem()}

	msgCh := make(chan *imap.Message, 20)
	done := make(chan error, 1)

	go func() {
		done <- src.Fetch(seqset, items, msgCh)
	}()

	for msg := range msgCh {
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, body)

		if err := dst.Append(mailbox, msg.Flags, msg.InternalDate, &buf); err != nil {
			return err
		}

		*moved++
		fmt.Printf("[%s] Progress: %d/%d (%.1f%%)\n",
			mailbox, *moved, total, float64(*moved)/float64(total)*100)
	}

	if err := <-done; err != nil {
		return err
	}
	return nil
}

// ---------------- utils ----------------

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func readPasswordMasked(prompt string) string {
	t, _ := tty.Open()
	defer t.Close()

	fmt.Print(prompt)
	var password []rune
	for {
		r, err := t.ReadRune()
		if err != nil {
			break
		}
		if r == '\r' || r == '\n' {
			break
		}
		if r == 127 || r == '\b' { // backspace
			if len(password) > 0 {
				password = password[:len(password)-1]
				fmt.Print("\b \b")
			}
		} else {
			password = append(password, r)
			fmt.Print("*")
		}
	}
	fmt.Println()
	return strings.TrimSpace(string(password))
}
