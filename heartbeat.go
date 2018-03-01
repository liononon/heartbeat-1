/*
Server Example:

	hbs := NewServer("my-secret", 15 * time.Second) // secret: my-secret, timeout: 15s
	hbs.OnConnect = func(identifier string) {
		fmt.Println(identifier, "is online")
	}
	hbs.OnDisconnect = func(identifier string) {
		fmt.Println(identifier, "is offline")
	}
	http.Handle("/heartbeat", hbs)

Client Example:

	cancel := &Client{
		ServerAddr: "http://hearbeat.example.com/heartbeat",
		Secret: "my-secret", // must be save with server secret
		Identifier: "client-unique-name",
	}.Beat(5 * time.Second)

	defer cancel() // cancel heartbeat
*/
package heartbeat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pkg/errors"
)

type Server struct {
	OnConnect    func(identifier string)
	OnDisconnect func(identifier string)
	hbTimeout    time.Duration
	secret       string // HMAC
	sessions     map[string]*Session
	mu           sync.Mutex
}

func NewServer(secret string, timeout time.Duration) *Server {
	return &Server{
		hbTimeout: timeout,
		secret:    secret,
		sessions:  make(map[string]*Session),
	}
}

func (s *Server) updateOrSaveSession(identifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[identifier]; ok {
		select {
		case sess.recvC <- "beat":
			log.Println(sess.identifier, "beat")
		default:
		}
	} else {
		if s.OnConnect != nil {
			s.OnConnect(identifier)
		}
		sess := &Session{
			identifier: identifier,
			timer:      time.NewTimer(s.hbTimeout),
			timeout:    s.hbTimeout,
			recvC:      make(chan string, 0),
		}
		s.sessions[identifier] = sess
		go func() {
			sess.drain()
			// delete session when timeout
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.OnDisconnect != nil {
				s.OnDisconnect(identifier)
			}
			delete(s.sessions, identifier)
		}()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	timestamp := r.FormValue("timestamp")
	identifier := r.FormValue("identifier")
	messageMAC := r.FormValue("messageMAC")
	if messageMAC != hashIdentifier(timestamp, identifier, s.secret) {
		http.Error(w, "messageMAC wrong", http.StatusBadRequest)
		return
	}
	if timestamp != "" {
		var t int64
		fmt.Sscanf(timestamp, "%d", &t)
		if time.Now().Unix()-t < 0 || time.Now().Unix()-t > int64(s.hbTimeout.Seconds()) {
			http.Error(w, "Invalid timestamp, advanced or outdated", http.StatusBadRequest)
			return
		}
		go s.updateOrSaveSession(identifier)
	}

	// send server timestamp to client
	t := time.Now().Unix()
	fmt.Fprintf(w, "%d %s", t, hashTimestamp(fmt.Sprintf("%d", t), s.secret))
}

type Session struct {
	identifier string
	timer      *time.Timer
	timeout    time.Duration
	recvC      chan string
}

func (sess *Session) drain() {
	log.Println("Drain")
	for {
		select {
		case <-sess.recvC:
			log.Println("timer reset", sess.timeout)
			sess.timer.Reset(sess.timeout)
		case <-sess.timer.C:
			log.Println("timeup")
			return
		}
	}
}

type Client struct {
	Secret     string
	Identifier string
	ServerAddr string
}

// Beat send identifier and hmac hash to server every interval
func (c *Client) Beat(interval time.Duration) (cancel context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.TODO())
	go func() {
		for {
			timeKey, err := c.httpBeat("", c.ServerAddr)
			if err != nil {
				log.Println(err)
				return
			}
			err = c.beatLoop(ctx, interval, timeKey)
			if err == nil {
				break
			}
		}
	}()
	return cancel
}

// send hearbeat continously
func (c *Client) beatLoop(ctx context.Context, interval time.Duration, timeKey string) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		newTimeKey, er := c.httpBeat(timeKey, c.ServerAddr)
		if er != nil {
			return errors.Wrap(er, "beatLoop")
		}
		timeKey = newTimeKey
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Client) httpBeat(serverTimeKey string, serverAddr string) (timeKey string, err error) {
	resp, err := http.PostForm(serverAddr, url.Values{ // TODO: http timeout
		"timestamp":  {serverTimeKey},
		"identifier": {c.Identifier},
		"messageMAC": {hashIdentifier(serverTimeKey, c.Identifier, c.Secret)}})
	if err != nil {
		err = errors.Wrap(err, "post form")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "ioutil readall")
		return
	}
	if resp.StatusCode != 200 {
		err = errors.New(string(body))
		return
	}

	// Receive server timestamp and check server hmac HASH
	var hashMAC string
	n, err := fmt.Sscanf(string(body), "%s %s", &timeKey, &hashMAC)
	if err != nil {
		log.Println(n, err)
		return
	}
	if hashTimestamp(timeKey, c.Secret) != hashMAC {
		err = errors.New("wrong timestamp hmac")
		return
	}
	return
}

func hashTimestamp(t, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s:timestamp", t)))
	return hex.EncodeToString(mac.Sum(nil))
}

func hashIdentifier(timestamp, identifier, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s:%s", timestamp, identifier)))
	return hex.EncodeToString(mac.Sum(nil))
}
