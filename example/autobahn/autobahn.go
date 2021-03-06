package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const dir = "./example/autobahn"

var (
	addr    = flag.String("listen", ":9001", "addr to listen")
	reports = flag.String("reports", dir+"/reports", "path to reports directory")
	static  = flag.String("static", dir+"/static", "path to static assets directory")
)

func main() {
	flag.Parse()

	log.Printf("reports dir is set to: %s", *reports)
	log.Printf("static dir is set to: %s", *static)

	http.HandleFunc("/", handlerIndex())
	http.HandleFunc("/library", handlerEcho())
	http.HandleFunc("/utils", handlerEcho2())
	http.Handle("/reports/", http.StripPrefix("/reports/", http.FileServer(http.Dir(*reports))))

	log.Printf("ready to listen on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

var (
	CloseInvalidPayload = ws.MustCompileFrame(
		ws.NewCloseFrame(ws.StatusInvalidFramePayloadData, ""),
	)
	CloseProtocolError = ws.MustCompileFrame(
		ws.NewCloseFrame(ws.StatusProtocolError, ""),
	)
)

func handlerEcho2() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.Upgrade(r, w, nil)
		if err != nil {
			log.Printf("upgrade error: %s", err)
			return
		}
		defer conn.Close()

		ch := wsutil.ControlHandler(conn, 0)

		rd := wsutil.NewReader(conn, ws.StateServerSide)
		rd.HandleIntermediate(ch)

		for {
			var r io.Reader = rd
			var ur *wsutil.UTF8Reader

			h, err := rd.Next()
			if err != nil {
				log.Printf("next reader error: %s", err)
				return
			}

			switch {
			case h.OpCode.IsControl():
				if err = ch(h, rd); err != nil {
					log.Print(err)
					return
				}
				continue

			case h.OpCode == ws.OpText:
				ur = wsutil.NewUTF8Reader(r)
				r = ur
			}

			wr := wsutil.NewWriter(conn, h.OpCode, false)
			_, err = io.Copy(wr, r)
			if err == nil && ur != nil {
				err = ur.Close()
			}
			if err == nil {
				err = wr.Flush()
			}
			if err != nil {
				log.Printf("copy error: %s", err)
				return
			}
		}
	}
}

func handlerEcho() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.Upgrade(r, w, nil)
		if err != nil {
			log.Printf("upgrade error: %s", err)
			return
		}
		defer conn.Close()

		state := ws.StateServerSide

		textPending := false
		utf8Reader := wsutil.NewUTF8Reader(nil)
		cipherReader := wsutil.NewCipherReader(nil, [4]byte{0, 0, 0, 0})

		for {
			header, err := ws.ReadHeader(conn)
			if err != nil {
				log.Printf("read header error: %s", err)
				break
			}
			if err = ws.CheckHeader(header, state); err != nil {
				log.Printf("header check error: %s", err)
				conn.Write(CloseProtocolError)
				return
			}

			var r io.Reader
			cipherReader.Reset(
				io.LimitReader(conn, header.Length),
				header.Mask,
			)
			r = cipherReader

			var utf8Fin bool
			switch header.OpCode {
			case ws.OpPing:
				header.OpCode = ws.OpPong
				header.Masked = false
				ws.WriteHeader(conn, header)
				io.CopyN(conn, cipherReader, header.Length)
				continue

			case ws.OpPong:
				io.CopyN(ioutil.Discard, conn, header.Length)
				continue

			case ws.OpClose:
				utf8Fin = true

			case ws.OpContinuation:
				if textPending {
					utf8Reader.SetSource(cipherReader)
					r = utf8Reader
				}
				if header.Fin {
					state = state.Clear(ws.StateFragmented)
					textPending = false
					utf8Fin = true
				}

			case ws.OpText:
				utf8Reader.Reset(cipherReader)
				r = utf8Reader

				if !header.Fin {
					state = state.Set(ws.StateFragmented)
					textPending = true
				} else {
					utf8Fin = true
				}

			case ws.OpBinary:
				if !header.Fin {
					state = state.Set(ws.StateFragmented)
				}
			}

			payload := make([]byte, header.Length)
			_, err = io.ReadFull(r, payload)
			if err == nil && utf8Fin {
				err = utf8Reader.Close()
			}
			if err != nil {
				log.Printf("read payload error: %s", err)
				if err == wsutil.ErrInvalidUtf8 {
					conn.Write(CloseInvalidPayload)
				} else {
					conn.Write(ws.CompiledClose)
				}
				return
			}

			if header.OpCode == ws.OpClose {
				code, reason := ws.ParseCloseFrameData(payload)
				log.Printf("close frame received: %v %v", code, reason)

				if !code.Empty() {
					switch {
					case code.IsProtocolSpec() && !code.IsProtocolDefined():
						err = fmt.Errorf("close code from spec range is not defined")
					default:
						err = ws.CheckCloseFrameData(code, reason)
					}
					if err != nil {
						log.Printf("invalid close data: %s", err)
						conn.Write(CloseProtocolError)
					} else {
						ws.WriteFrame(conn, ws.NewCloseFrame(code, ""))
					}
					return
				}

				conn.Write(ws.CompiledClose)
				return
			}

			header.Masked = false
			ws.WriteHeader(conn, header)
			conn.Write(payload)
		}
	}
}

func handlerIndex() func(w http.ResponseWriter, r *http.Request) {
	index, err := os.Open(*static + "/index.html")
	if err != nil {
		log.Fatal(err)
	}
	bts, err := ioutil.ReadAll(index)
	if err != nil {
		log.Fatal(err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("reqeust to %s", r.URL)
		switch r.URL.Path {
		case "/":
			buf := bytes.NewBuffer(bts)
			_, err := buf.WriteTo(w)
			if err != nil {
				log.Printf("write index bytes error: %s", err)
			}
		case "/favicon.ico":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}
