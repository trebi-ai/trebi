package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

// Limits of the adapter folder contract.
const (
	// socketMax is the size limit of a unix socket path.
	socketMax = 104
	// stateDirMax is the longest TREBI_STATE_DIR on a normal home: a
	// connection folder of 78 bytes plus "/state".
	stateDirMax = 78 + len("/state")
	// scanMax is the largest state file the relative path check reads.
	scanMax = 64 << 20
)

// folderSuite checks the adapter folder contract. It starts the program
// four times: on empty folders, again on the same state, on a copy of the
// state under a new HOME, and on the same state with a new cache.
type folderSuite struct{}

func (folderSuite) name() string { return SuiteFolder }

func (folderSuite) applies(m manifest.Manifest) bool {
	return m.Protocol() == manifest.ProtocolConnector
}

func (folderSuite) run(ctx context.Context, r *run) []Result {
	if r.live {
		return []Result{{Suite: SuiteFolder, Check: "folders", Status: StatusSkip, Message: "checked in the sandbox only"}}
	}
	d1, err := r.folders()
	if err != nil {
		return []Result{row(SuiteFolder, "folders", err)}
	}
	d3, err := r.folders()
	if err != nil {
		return []Result{row(SuiteFolder, "folders", err)}
	}
	key := "conformance-folder-" + strconv.FormatInt(r.now().UnixNano(), 36)
	first := r.visit(ctx, d1, true, key)
	again := r.visit(ctx, d1, false, key)
	d3.state = filepath.Join(r.root, "restore", "state")
	if err := copyTree(d1.state, d3.state); err != nil {
		return []Result{row(SuiteFolder, "restore", err)}
	}
	restored := r.visit(ctx, d3, false, key)
	cacheErr := os.RemoveAll(d1.cache)
	if cacheErr == nil {
		cacheErr = os.MkdirAll(d1.cache, 0o700)
	}
	noCache := r.visit(ctx, d1, false, "")

	login := r.t.Manifest.Setup.AdapterLogin()
	out := []Result{
		row(SuiteFolder, "writes stay inside", emptyHomes(d1.home, d3.home)),
		row(SuiteFolder, "relative paths", relativeState(d1.state, d3.state)),
	}
	if first.start != nil {
		out = append(out,
			Result{Suite: SuiteFolder, Check: "clean stop", Status: StatusSkip, Message: "the first run did not start"},
			Result{Suite: SuiteFolder, Check: "sockets", Status: StatusSkip, Message: "the first run did not start"})
	} else {
		out = append(out, row(SuiteFolder, "clean stop", errors.Join(first.stop, walFiles(d1.state))), row(SuiteFolder, "sockets", first.sockets))
	}
	return append(out,
		row(SuiteFolder, "restart keeps state", again.keeps(first, login)),
		row(SuiteFolder, "restore", restored.keeps(first, login)),
		row(SuiteFolder, "empty state", first.emptyState(login)),
		row(SuiteFolder, "cache can go", errors.Join(cacheErr, noCache.started())))
}

// visit is what one start of the program showed.
type visit struct {
	start   error // start or initialize failed
	status  protocol.Status
	hasSt   bool
	exit    string // the program exited after its status
	login   error
	msgID   string
	send    error
	sockets error
	stop    error
}

// visit starts the program on d, waits for its status, logs in when login
// is set, sends one message with key when key is set, and shuts it down.
func (r *run) visit(ctx context.Context, d dirs, login bool, key string) visit {
	var v visit
	chk := &protocol.Checker{}
	a, err := r.start(d, r.inputs(), chk.Notify, chk.BadLine)
	if err != nil {
		v.start = err
		return v
	}
	defer a.stop()
	if _, err := chk.Initialize(ctx, a.conn); err != nil {
		if a.waitExit(aliveWait) {
			err = fmt.Errorf("%w; %s", err, a.exitMessage())
		}
		v.start = err
		return v
	}
	v.status, v.hasSt = chk.FirstStatus(ctx)
	if a.waitExit(aliveWait) {
		v.exit = a.exitMessage()
		return v
	}
	if login && v.status.State == protocol.StateAuthRequired {
		v.login = chk.LogIn(ctx, a.conn)
	}
	if key != "" {
		v.msgID, v.send = r.sendKey(ctx, a.conn, key)
	}
	v.sockets = longSockets(d.state)
	sctx, cancel := context.WithTimeout(ctx, stopWait)
	defer cancel()
	if err := a.conn.Call(sctx, protocol.MethodShutdown, protocol.Empty{}, nil); err != nil {
		v.stop = fmt.Errorf("shutdown: %w", err)
	} else if !a.waitExit(stopWait) {
		v.stop = fmt.Errorf("the program did not exit in %s after shutdown", stopWait)
	}
	return v
}

// sendKey sends one message with key to the first room, or to the test
// room. It returns "" with no room.
func (r *run) sendKey(ctx context.Context, conn protocol.Caller, key string) (string, error) {
	room := r.t.Room
	if room == "" && slices.Contains(r.t.Manifest.Features(), manifest.FeatureRoomsList) {
		var page protocol.RoomPage
		if err := conn.Call(ctx, protocol.MethodRoomsList, protocol.RoomsListParams{Limit: 1}, &page); err != nil {
			return "", fmt.Errorf("rooms/list: %w", err)
		}
		if len(page.Rooms) > 0 {
			room = page.Rooms[0].ID
		}
	}
	if room == "" {
		return "", nil
	}
	var res protocol.SendResult
	err := conn.Call(ctx, protocol.MethodMessagesSend, protocol.SendParams{Room: room, Text: "Trebi conformance folder check", Format: "text", Key: key}, &res)
	if err != nil {
		return "", fmt.Errorf("messages/send: %w", err)
	}
	return res.MessageID, nil
}

// started is the error of a visit that did not start or did not stay.
func (v visit) started() error {
	switch {
	case v.start != nil:
		return v.start
	case !v.hasSt:
		return errors.New("no status after initialize")
	case v.exit != "":
		return errors.New(v.exit)
	case v.status.State == protocol.StateError:
		return fmt.Errorf("status error: %s", v.status.Message)
	}
	return nil
}

// emptyState checks the first visit: the program stays alive and asks for
// a login, or connects.
func (v visit) emptyState(login bool) error {
	if v.start != nil {
		return fmt.Errorf("on an empty state folder: %w", v.start)
	}
	if err := v.started(); err != nil {
		return err
	}
	switch st := v.status.State; {
	case login && st != protocol.StateAuthRequired:
		return fmt.Errorf("on an empty state folder, the first status is %q, want %s", st, protocol.StateAuthRequired)
	case !login && st != protocol.StateConnecting && st != protocol.StateConnected:
		return fmt.Errorf("on an empty state folder, the first status is %q, want %s", st, protocol.StateConnecting)
	}
	return nil
}

// keeps checks a later visit against the first: the login stayed, and a
// send with the key of the first visit gave the same message id.
func (v visit) keeps(first visit, login bool) error {
	if err := v.started(); err != nil {
		return err
	}
	var errs []error
	if login {
		if first.login != nil {
			errs = append(errs, fmt.Errorf("the sandbox login of the first run failed: %w", first.login))
		} else if v.status.State == protocol.StateAuthRequired {
			errs = append(errs, errors.New("the login did not stay: the first status is auth_required"))
		}
	}
	switch {
	case first.send != nil:
		errs = append(errs, fmt.Errorf("the send of the first run failed: %w", first.send))
	case first.msgID == "":
	case v.send != nil:
		errs = append(errs, v.send)
	case v.msgID != first.msgID:
		errs = append(errs, fmt.Errorf("a send with the key of the first run gave message id %q, not %q: the dedupe file did not stay", v.msgID, first.msgID))
	}
	return errors.Join(errs...)
}

// emptyHomes fails when a HOME folder holds anything.
func emptyHomes(homes ...string) error {
	var found []string
	for _, h := range homes {
		entries, err := os.ReadDir(h)
		if err != nil {
			return err
		}
		for _, e := range entries {
			found = append(found, "$HOME/"+e.Name())
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("the program wrote outside TREBI_STATE_DIR and TREBI_CACHE_DIR: %s", strings.Join(found, ", "))
	}
	return nil
}

// relativeState fails when a file in a state folder holds the absolute path
// of that folder.
func relativeState(states ...string) error {
	var found []string
	for _, state := range states {
		needles := [][]byte{[]byte(state)}
		if real, err := filepath.EvalSymlinks(state); err == nil && real != state {
			needles = append(needles, []byte(real))
		}
		err := filepath.WalkDir(state, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close() //nolint:errcheck // read only
			b, err := io.ReadAll(io.LimitReader(f, scanMax))
			if err != nil {
				return err
			}
			if slices.ContainsFunc(needles, func(n []byte) bool { return bytes.Contains(b, n) }) {
				rel, _ := filepath.Rel(state, p) //nolint:errcheck // p is under state
				found = append(found, rel)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("state files hold the absolute state path, so a restore to another path breaks: %s", strings.Join(slices.Compact(found), ", "))
	}
	return nil
}

// walFiles fails when a SQLite WAL file in the state folder is not empty
// after the program stopped.
func walFiles(state string) error {
	var found []string
	err := filepath.WalkDir(state, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), "-wal") {
			return err
		}
		if fi, err := e.Info(); err == nil && fi.Size() > 0 {
			rel, _ := filepath.Rel(state, p) //nolint:errcheck // p is under state
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("WAL files are not empty after shutdown; checkpoint and close the database: %s", strings.Join(found, ", "))
	}
	return nil
}

// longSockets fails when a socket in the state folder would pass the unix
// path limit under the longest state folder of the daemon.
func longSockets(state string) error {
	var found []string
	err := filepath.WalkDir(state, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.Type()&fs.ModeSocket == 0 {
			return err
		}
		rel, _ := filepath.Rel(state, p) //nolint:errcheck // p is under state
		if stateDirMax+1+len(rel) >= socketMax {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("socket paths pass %d bytes under a %d-byte state folder: %s", socketMax, stateDirMax, strings.Join(found, ", "))
	}
	return nil
}

// copyTree copies the regular files and folders of src to dst, as a
// restore does.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case !e.Type().IsRegular():
			return nil // a socket or a lock belongs to the old process
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}
