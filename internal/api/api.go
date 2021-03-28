package api

import (
	"net"
	"net/http"
	"strconv"

	"github.com/aler9/rtsp-simple-server/internal/logger"
)

func muxHandle(mux *http.ServeMux, method string, path string,
	cb func(w http.ResponseWriter, r *http.Request)) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cb(w, r)
	})
}

type GetConfRes struct {
}

type GetConfReq struct {
	res chan GetConfRes
}

// Parent is implemented by program.
type Parent interface {
	Log(logger.Level, string, ...interface{})
}

type API struct {
	s *http.Server
}

func New(
	listenIP string,
	port int,
	parent Parent,
) (*API, error) {

	a := &API{}

	address := listenIP+":"+strconv.FormatInt(int64(port), 10)
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	muxHandle(mux, "GET", "/config", a.onGETConfig)

	// POST /config
	// GET /config/paths
	// GET /config/path/id
	// POST /config/path/id
	// GET /paths
	// GET /clients
	// POST /client/id/kick

	a.s = &http.Server{
		Handler: mux,
	}

	go a.s.Serve(ln)

	parent.Log(logger.Info, "[api] opened on "+address)

	return a, nil
}

func (a *API) Close() {
	a.s.Close()
}

func (a *API) onGETConfig(w http.ResponseWriter, r *http.Request) {
	res := make(chan GetConfRes)
	a.parent.OnAPI(GetConfReq{Res: res})
	<-res

	w.Write([]byte("OK\n"))
}
