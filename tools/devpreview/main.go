// Command devpreview serves the gvm dashboard on stubbed data so the pages
// can be eyeballed in a browser without Postgres or gvmd: every request is
// auto-authenticated as a stub operator, the instance list is fixtures, and
// the console streams fake lines. Development only — never deployed.
//
//	go run ./tools/devpreview   # then open http://127.0.0.1:8130/instances
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	consolepkg "github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/web"
)

type sessions struct{}

func (sessions) LookupSession(_ context.Context, id string) (*auth.Session, error) {
	return &auth.Session{ID: id, UserID: "u1", Email: "operator@example.com", DisplayName: "", Role: "operator"}, nil
}

type tokens struct{}

func (tokens) LookupAPIToken(context.Context, []byte) (*auth.APIToken, error) {
	return nil, auth.ErrTokenNotFound
}
func (tokens) RecordTokenUse(context.Context, string, time.Time, string) error { return nil }

type instances struct{}

var fixtures = []web.InstanceSummary{
	{Name: "pz-main", Game: "projectzomboid", Status: "Running."},
	{Name: "valheim-longname-eu-west-3b", Game: "valheim", Status: "This server keeps crashing as it starts. The server operator needs to check the logs."},
	{Name: "mc-creative", Game: "minecraft", Status: "The game files are being installed. This can take a while."},
}

func (instances) ListInstances(context.Context) ([]web.InstanceSummary, error) {
	return fixtures, nil
}

func (instances) GetInstance(_ context.Context, name string) (*web.InstanceDetail, error) {
	for _, f := range fixtures {
		if f.Name == name {
			return &web.InstanceDetail{Name: f.Name, Game: f.Game, Status: f.Status}, nil
		}
	}
	return nil, fmt.Errorf("web: no such instance")
}

type dispatcher struct{}

func (dispatcher) Start(context.Context, string, string) (string, error)   { return "op_dev", nil }
func (dispatcher) Stop(context.Context, string, string) (string, error)    { return "op_dev", nil }
func (dispatcher) Restart(context.Context, string, string) (string, error) { return "op_dev", nil }
func (dispatcher) ConsoleSend(context.Context, string, string, string) error {
	return nil
}

type console struct{}

func (console) Stream(ctx context.Context, instance string, from uint64, backlog int, emit func(consolepkg.Event)) error {
	seq := from
	if seq == 0 {
		seq = 1
	}
	var batch []consolepkg.Line
	for i := 0; i < 25; i++ {
		batch = append(batch, consolepkg.Line{Seq: seq, At: time.Now(),
			Text: fmt.Sprintf("[dev] boot line %d — a fairly long console line to exercise wrapping behaviour on narrow phone screens without a terminator", seq)})
		seq++
	}
	emit(consolepkg.Event{Ev: consolepkg.EvLines, Lines: batch})
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			emit(consolepkg.Event{Ev: consolepkg.EvLines, Lines: []consolepkg.Line{
				{Seq: seq, At: time.Now(), Text: fmt.Sprintf("[dev] tick %d", seq)}}})
			seq++
		}
	}
}

func main() {
	s, err := web.New(web.Config{
		Addr:            "127.0.0.1:8130",
		SessionQuerier:  sessions{},
		TokenQuerier:    tokens{},
		InstanceQuerier: instances{},
		Dispatcher:      dispatcher{},
		Console:         console{},
		InsecureCookies: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	h := s.Handler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auto-authenticate every request with a dev session cookie EXCEPT the
		// login pages, so those still render their logged-out forms (the smoke
		// test renders /login; an injected session would just redirect it away).
		if !strings.HasPrefix(r.URL.Path, "/login") {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "dev"})
		}
		h.ServeHTTP(w, r)
	})
	log.Println("devpreview on http://127.0.0.1:8130/instances")
	log.Fatal(http.ListenAndServe("127.0.0.1:8130", wrapped))
}
