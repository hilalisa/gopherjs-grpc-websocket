package wsproxy

import (
	"bufio"
	"io"
	"net/http"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
)

var (
	MethodOverrideParam = "method"
	TokenCookieName     = "token"
)

// WebsocketProxy attempts to expose the underlying handler as a bidi websocket stream with newline-delimited
// JSON as the content encoding.
//
// The HTTP Authorization header is either populated from the Sec-Websocket-Protocol field or by a cookie.
// The cookie name is specified by the TokenCookieName value.
//
// example:
//   Sec-Websocket-Protocol: Bearer, foobar
// is converted to:
//   Authorization: Bearer foobar
//
// Method can be overwritten with the MethodOverrideParam get parameter in the requested URL
func WebsocketProxy(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			h.ServeHTTP(w, r)
			return
		}
		websocketProxy(w, r, h)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func websocketProxy(w http.ResponseWriter, r *http.Request, h http.Handler) {
	var responseHeader http.Header
	// If Sec-WebSocket-Protocol starts with "Bearer", respond in kind.
	if strings.HasPrefix(r.Header.Get("Sec-WebSocket-Protocol"), "Bearer") {
		responseHeader = http.Header{
			"Sec-WebSocket-Protocol": []string{"Bearer"},
		}
	}
	conn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		log.Warnln("error upgrading websocket:", err)
		return
	}
	defer conn.Close()

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	requestBodyR, requestBodyW := io.Pipe()
	request, err := http.NewRequest(r.Method, r.URL.String(), requestBodyR)
	if err != nil {
		log.Warnln("error preparing request:", err)
		return
	}
	if swsp := r.Header.Get("Sec-WebSocket-Protocol"); swsp != "" {
		request.Header.Set("Authorization", strings.Replace(swsp, "Bearer, ", "Bearer ", 1))
	}
	// If token cookie is present, populate Authorization header from the cookie instead.
	if cookie, err := r.Cookie(TokenCookieName); err == nil {
		request.Header.Set("Authorization", "Bearer "+cookie.Value)
	}
	if m := r.URL.Query().Get(MethodOverrideParam); m != "" {
		request.Method = m
	}

	responseBodyR, responseBodyW := io.Pipe()
	go func() {
		<-ctx.Done()
		log.Debugln("closing pipes")
		requestBodyW.CloseWithError(io.EOF)
		responseBodyW.CloseWithError(io.EOF)
	}()

	response := newInMemoryResponseWriter(responseBodyW)
	go func() {
		defer cancelFn()
		h.ServeHTTP(response, request)
	}()

	// read loop -- take messages from websocket and write to http request
	go func() {
		defer func() {
			cancelFn()
		}()
		for {
			select {
			case <-ctx.Done():
				log.Debugln("read loop done")
				return
			default:
			}
			log.Debugln("[read] reading from socket.")
			_, p, err := conn.ReadMessage()
			if err != nil {
				log.Warnln("error reading websocket message:", err)
				return
			}
			log.Debugln("[read] read payload:", string(p))
			log.Debugln("[read] writing to requestBody:")
			n, err := requestBodyW.Write(p)
			requestBodyW.Write([]byte("\n"))
			log.Debugln("[read] wrote to requestBody", n)
			if err != nil {
				log.Warnln("[read] error writing message to upstream http server:", err)
				return
			}
		}
	}()
	// write loop -- take messages from response and write to websocket
	scanner := bufio.NewScanner(responseBodyR)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			log.Warnln("[write] empty scan", scanner.Err())
			continue
		}
		log.Debugln("[write] scanned", scanner.Text())
		if err = conn.WriteMessage(websocket.TextMessage, scanner.Bytes()); err != nil {
			log.Warnln("[write] error writing websocket message:", err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Warnln("scanner err:", err)
	}
}

type inMemoryResponseWriter struct {
	io.Writer
	header http.Header
	code   int
}

func newInMemoryResponseWriter(w io.Writer) *inMemoryResponseWriter {
	return &inMemoryResponseWriter{
		Writer: w,
		header: http.Header{},
	}
}

func (w *inMemoryResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}
func (w *inMemoryResponseWriter) Header() http.Header {
	return w.header
}
func (w *inMemoryResponseWriter) WriteHeader(code int) {
	w.code = code
}
func (w *inMemoryResponseWriter) Flush() {}
