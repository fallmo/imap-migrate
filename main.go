package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

var folderMap = map[string]string{
	"INBOX":             "INBOX",
	"[Gmail]/Sent Mail": "Sent",
	"[Gmail]/Drafts":    "Drafts",
	"[Gmail]/Spam":      "Junk",
	"[Gmail]/Trash":     "Trash",
	"[Gmail]/All Mail":  "Archive",
}

func resolveMailbox(name string) string {
	if mapped, ok := folderMap[name]; ok {
		return mapped
	}
	return name
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	srcServer := "imap.gmail.com:993"
	dstServer := "smtp.heritage.africa:993"
	dstPass := "Accel@2025"
	mboxPattern := "*"
	batchSize := 200

	var srcUser string
	var srcPass string
	var reusingCache bool

	fmt.Println("Welcome fellow change maker. This program will copy your mails from gmail onto heritage. Before we begin, we will need some information.")

	cachedUserData := getCacheData()
	if cachedUserData != nil {
		fmt.Printf("Reuse existing email and app assword for '%s'? (yes/no): (default yes)", cachedUserData.Email)
		reuse, _ := reader.ReadString('\n')
		reuse = strings.TrimSpace(reuse)
		switch reuse {
		case "", "yes":
			reusingCache = true
			srcUser = cachedUserData.Email
			srcPass = cachedUserData.AppPassword
		case "no":
			clearCachedData()
			reusingCache = false
		default:
			fmt.Println("Expects 'yes' or 'no'")
			os.Exit(1)
		}

	}

	if !reusingCache {
		fmt.Print("Your email: ")
		srcUser, _ = reader.ReadString('\n')
		srcUser = strings.TrimSpace(srcUser)

		fmt.Print("Your google app password: ")
		srcPass, _ = reader.ReadString('\n')
		srcPass = strings.TrimSpace(srcPass)
	}

	fmt.Println("Connecting to google...")
	src, err := dialIMAP(srcServer, srcUser, srcPass, true)
	check(err)
	defer src.Logout()

	fmt.Println("Connecting to heritage...")
	dst, err := dialIMAP(dstServer, srcUser, dstPass, true)
	check(err)
	defer dst.Logout()

	setCacheData(srcUser, srcPass)
	fmt.Print("\nConnections Succesful.\nCredentials cached for subsequent executions.\nPress enter to begin mail synchronization\nThis process may take up to 45 minutes. In case of an error, just rerun the program.")
	_, _ = reader.ReadString('\n')

	fmt.Println("Listing mailboxes...")
	mboxes := listMailboxes(src, mboxPattern)

	for _, m := range mboxes {
		fmt.Printf("Syncing mailbox: %s\n", m)
		moved, skipped, err := syncMailbox(src, dst, m, batchSize)
		if err != nil {
			log.Printf("Error syncing %s: %v", m, err)
			continue
		}
		fmt.Printf("Done %s: moved %d messages, and skipped %d messages\n", m, moved, skipped)
	}

	fmt.Println("Sync complete!")
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

func syncMailbox(src, dst *client.Client, name string, batchSize int) (int, int, error) {
	dstName := resolveMailbox(name)

	mbox, err := src.Select(name, true)
	if err != nil {
		return 0, 0, err
	}
	if mbox.Messages == 0 {
		fmt.Printf("[%s] Mailbox is empty\n", name)
		return 0, 0, nil
	}
	if err := ensureMailbox(dst, dstName); err != nil {
		return 0, 0, err
	}
	if _, err := dst.Select(dstName, false); err != nil {
		return 0, 0, err
	}

	total := int(mbox.Messages)
	fmt.Printf("[%s -> %s] Mails total count: %d\n", name, dstName, total)

	moved := 0
	skipped := 0
	var from uint32 = 1
	for from <= mbox.Messages {
		to := from + uint32(batchSize) - 1
		if to > mbox.Messages {
			to = mbox.Messages
		}
		if err := copyBatch(src, dst, name, from, to, total, &moved, &skipped); err != nil {
			return moved, skipped, err
		}
		from = to + 1
	}
	return moved, skipped, nil
}

func ensureMailbox(c *client.Client, name string) error {
	_, err := c.Select(name, true)
	if err == nil {
		return nil
	}
	return c.Create(name)
}

func copyBatch(src, dst *client.Client, mailbox string, from, to uint32, total int, moved *int, skipped *int) error {
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, to)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, section.FetchItem()}

	msgCh := make(chan *imap.Message, 20)
	done := make(chan error, 1)

	go func() {
		done <- src.Fetch(seqset, items, msgCh)
	}()

	dstBox := resolveMailbox(mailbox)

	for msg := range msgCh {
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, body)

		// Get Message-ID (if available)
		msgID := ""
		if msg.Envelope != nil {
			msgID = msg.Envelope.MessageId
		}

		// Skip if already exists in destination
		if msgID != "" && messageExists(dst, dstBox, msgID) {
			fmt.Printf("[%s] Skipping existing: %s\n", dstBox, msgID)
			*skipped++
			continue
		}

		// Append if not present
		if err := dst.Append(dstBox, msg.Flags, msg.InternalDate, &buf); err != nil {
			return err
		}

		*moved++
		fmt.Printf("[%s] Progress: %d/%d (%.1f%%)\n",
			dstBox, *moved+*skipped, total, float64(*moved+*skipped)/float64(total)*100)
	}

	if err := <-done; err != nil {
		return err
	}
	return nil
}

func messageExists(c *client.Client, _, msgID string) bool {
	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("Message-ID", msgID)

	ids, err := c.Search(criteria)
	if err != nil {
		return false
	}
	return len(ids) > 0
}

// ---------------- utils ----------------

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const cacheFileName = ".imap_data_cache.json"

type cachedData struct {
	Email       string `json:"email"`
	AppPassword string `json:"app_password"`
}

func getCacheData() *cachedData {

	bytes, err := os.ReadFile(cacheFileName)
	if err != nil {
		return nil
	}

	var data cachedData

	err = json.Unmarshal(bytes, &data)
	if err != nil {
		return nil
	}

	return &data

}

func setCacheData(email string, password string) error {
	data := cachedData{
		Email:       email,
		AppPassword: password,
	}

	bytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	err = os.WriteFile(cacheFileName, bytes, 0660)
	return err
}

func clearCachedData() error {
	err := os.Remove(cacheFileName)

	return err
}
