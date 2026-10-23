package connectors

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/procgroup"
)

// Owners of a login method.
const (
	OwnerDaemon  = "daemon"  // a manifest method with an input
	OwnerAdapter = "adapter" // a method that the adapter answers in auth/methods
)

// Reasons of an unavailable adapter method.
const (
	reasonNotRunning = "Connect the connector first."
	reasonNotOffered = "This version of the connector does not offer this login."
)

const (
	// keyCheckWait bounds the check of a pasted key: the restart and the
	// first status, or the test command.
	keyCheckWait = 30 * time.Second
	// keyTestOutput is how much test output the error keeps.
	keyTestOutput = 4 << 10
)

// LoginMethod is one way to log in to a connection, with who runs it.
type LoginMethod struct {
	protocol.LoginMethod
	Owner string `json:"owner"`
}

// LoginMethods merges the manifest methods with the live answer of
// auth/methods. A daemon method is always available. An adapter method is
// not available while the process does not run. A live method that the
// manifest does not declare comes after the declared ones. The list is in
// the type order of the UI, then in the manifest order.
func (s *Service) LoginMethods(ctx context.Context, ref string) ([]LoginMethod, error) {
	r, err := s.Resolve(ref, "")
	if err != nil {
		return nil, err
	}
	live, liveErr := s.adapterMethods(ctx, r)
	order := protocol.LoginTypes()
	out := []LoginMethod{}
	var declared []manifest.LoginMethod
	if r.Manifest.Setup != nil {
		declared = r.Manifest.Setup.Login
	}
	for _, d := range declared {
		m := LoginMethod{LoginMethod: protocol.LoginMethod{ID: d.ID, Type: d.Type, Label: d.Label, Help: d.Help}, Owner: OwnerAdapter}
		switch i := slices.IndexFunc(live, func(l protocol.LoginMethod) bool { return l.ID == d.ID }); {
		case d.Daemon():
			m.Owner, m.Available = OwnerDaemon, s.oauth != nil || d.OAuth == nil
		case i >= 0:
			m.Available, m.Reason = live[i].Available, live[i].Reason
			m.Label, m.Help = cmp.Or(live[i].Label, m.Label), cmp.Or(live[i].Help, m.Help)
		case liveErr != nil:
			m.Reason = reasonNotRunning
		default:
			m.Reason = reasonNotOffered
		}
		out = append(out, m)
	}
	if m, ok := s.oauthMethod(r); ok && !slices.ContainsFunc(declared, func(d manifest.LoginMethod) bool { return d.OAuth != nil }) {
		out = append(out, LoginMethod{LoginMethod: protocol.LoginMethod{ID: m.ID, Type: m.Type, Label: m.Label, Available: true}, Owner: OwnerDaemon})
	}
	for _, l := range live {
		if slices.Contains(order, l.Type) && !slices.ContainsFunc(declared, func(d manifest.LoginMethod) bool { return d.ID == l.ID }) {
			out = append(out, LoginMethod{LoginMethod: l, Owner: OwnerAdapter})
		}
	}
	for i := range out {
		out[i].Label = cmp.Or(out[i].Label, defaultLoginLabel(out[i].Type, r.Title))
	}
	slices.SortStableFunc(out, func(a, b LoginMethod) int {
		return slices.Index(order, a.Type) - slices.Index(order, b.Type)
	})
	return out, nil
}

// adapterMethods asks the live session for its methods. It returns nil
// when the manifest declares no adapter method, and an error when the
// process does not run or does not answer.
func (s *Service) adapterMethods(ctx context.Context, r Resolved) ([]protocol.LoginMethod, error) {
	if !r.Manifest.Setup.AdapterLogin() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, protocol.DefaultTimeout)
	defer cancel()
	var res protocol.AuthMethodsResult
	if err := s.Call(ctx, r.ID, protocol.MethodAuthMethods, protocol.Empty{}, &res); err != nil {
		s.log.Debug("connector.auth_methods", "connector", r.ID, "err", err)
		return nil, err
	}
	return res.Methods, nil
}

// defaultLoginLabel is the label of a method that names none.
func defaultLoginLabel(typ, title string) string {
	switch typ {
	case protocol.LoginExisting:
		return "Use existing login"
	case protocol.LoginBrowser:
		return "Log in with " + title
	case protocol.LoginPhone:
		return "Link your phone"
	}
	return "Paste a key"
}

// PickLoginMethod returns the named method, or the first available one for
// an empty name. An unknown or unavailable method is invalid.
func (s *Service) PickLoginMethod(ctx context.Context, ref, method string) (LoginMethod, error) {
	methods, err := s.LoginMethods(ctx, ref)
	if err != nil {
		return LoginMethod{}, err
	}
	i := slices.IndexFunc(methods, func(m LoginMethod) bool {
		return m.ID == method || (method == "" && m.Available)
	})
	switch {
	case i < 0 && method == "":
		return LoginMethod{}, protocol.NewError(protocol.CodeInvalid, "No login method is available.")
	case i < 0:
		return LoginMethod{}, protocol.NewError(protocol.CodeInvalid, fmt.Sprintf("unknown login method %q", method))
	case !methods[i].Available:
		return LoginMethod{}, protocol.NewError(protocol.CodeInvalid, methods[i].Reason)
	}
	return methods[i], nil
}

// Login starts a login flow with one method and returns the flow with its
// first step. An empty method picks the first available one.
func (s *Service) Login(ctx context.Context, ref, method string) (*Flow, error) {
	m, err := s.PickLoginMethod(ctx, ref, method)
	if err != nil {
		return nil, err
	}
	instance := s.IDOf(ref)
	s.pruneFlows()
	if m.Owner == OwnerDaemon && m.Type == protocol.LoginBrowser {
		return s.beginOAuthLogin(ctx, instance, m)
	}
	if m.Owner == OwnerDaemon {
		return s.beginKeyLogin(instance, m)
	}
	ctx, cancel := context.WithTimeout(ctx, loginWait)
	defer cancel()
	var res protocol.AuthBeginResult
	if err := s.Call(ctx, instance, protocol.MethodAuthBegin, protocol.AuthBeginParams{Method: m.ID}, &res); err != nil {
		return nil, err
	}
	if res.FlowID == "" {
		return nil, protocol.NewError(protocol.CodeInvalid, "auth/begin returned no flow_id")
	}
	f := s.flow(instance, res.FlowID, true)
	f.setMethod(m.LoginMethod, nil)
	f.setStep(res.Step, true)
	return f, nil
}

// SubmitLogin answers an input step, or the fields of a url step.
func (s *Service) SubmitLogin(ctx context.Context, f *Flow, fields map[string]protocol.Value) error {
	if of := f.oauthFlow(); of != nil {
		return s.submitOAuthLogin(ctx, f, of, fields)
	}
	if key := f.keyMethod(); key != nil {
		return s.submitKeyLogin(ctx, f, *key, fields)
	}
	var res protocol.AuthSubmitResult
	if err := s.Call(ctx, f.Instance, protocol.MethodAuthSubmit, protocol.AuthSubmitParams{FlowID: f.ID, Fields: fields}, &res); err != nil {
		return err
	}
	if res.Step != nil {
		f.setStep(*res.Step, false)
	}
	return nil
}

// CancelLogin ends a flow.
func (s *Service) CancelLogin(ctx context.Context, f *Flow) error {
	var err error
	if of := f.oauthFlow(); of != nil {
		s.dropState(of.State)
	} else if f.keyMethod() == nil {
		err = s.Call(ctx, f.Instance, protocol.MethodAuthCancel, protocol.AuthFlowParams{FlowID: f.ID}, nil)
	}
	f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: "cancelled"})
	return err
}

// beginKeyLogin starts a daemon key flow: one secret field made from the
// input of the method.
func (s *Service) beginKeyLogin(instance string, m LoginMethod) (*Flow, error) {
	r, err := s.Resolve(instance, "")
	if err != nil {
		return nil, err
	}
	d, in, ok := keyMethodOf(r.Manifest, m.ID)
	if !ok {
		return nil, protocol.NewError(protocol.CodeInvalid, fmt.Sprintf("login method %q has no input", m.ID))
	}
	f := s.flow(instance, "key_"+strings.ToLower(ulid.Make().String()), true)
	f.setMethod(m.LoginMethod, &d)
	f.setStep(protocol.Step{Kind: protocol.StepInput, Fields: []protocol.StepField{{
		Name: in.Name, Label: cmp.Or(in.Label, "Key"), Secret: true, Help: in.Help, URL: in.URL,
	}}}, true)
	return f, nil
}

// keyMethodOf returns a daemon method of a manifest and its input.
func keyMethodOf(m manifest.Manifest, id string) (manifest.LoginMethod, manifest.Input, bool) {
	if m.Setup == nil {
		return manifest.LoginMethod{}, manifest.Input{}, false
	}
	i := slices.IndexFunc(m.Setup.Login, func(l manifest.LoginMethod) bool { return l.ID == id && l.Input != "" })
	if i < 0 {
		return manifest.LoginMethod{}, manifest.Input{}, false
	}
	d := m.Setup.Login[i]
	j := slices.IndexFunc(m.Setup.Inputs, func(in manifest.Input) bool { return in.Name == d.Input })
	if j < 0 {
		return manifest.LoginMethod{}, manifest.Input{}, false
	}
	return d, m.Setup.Inputs[j], true
}

// submitKeyLogin saves the pasted key as the secret input, restarts the
// connection, and checks the key. The flow ends with the result. The value
// never goes to the bus or to a log.
func (s *Service) submitKeyLogin(ctx context.Context, f *Flow, d manifest.LoginMethod, fields map[string]protocol.Value) error {
	value := strings.TrimSpace(fields[d.Input].String())
	if value == "" {
		return protocol.NewError(protocol.CodeInvalid, "Paste the key first.")
	}
	f.setStep(protocol.Step{Kind: protocol.StepWait, Message: "Trebi checks the key."}, false)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyCheckWait)
	defer cancel()
	before := s.procByName(f.Instance)
	if err := s.setSecretInput(ctx, f.Instance, d.Input, value); err != nil {
		f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: err.Error()})
		return err
	}
	err := s.checkKey(ctx, f.Instance, d, before)
	s.setKeyCheck(f.Instance, err)
	if err != nil {
		f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: err.Error()})
		return nil
	}
	f.finish(protocol.AuthDoneParams{FlowID: f.ID, OK: true})
	s.emitConnector("connector.auth.completed", f.Instance, nil)
	return nil
}

// setSecretInput writes one secret input and keeps the other inputs. The
// write restarts a running connection, because its env changes.
func (s *Service) setSecretInput(ctx context.Context, ref, name, value string) error {
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	inputs := maps.Clone(c.Inputs)
	if inputs == nil {
		inputs = map[string]string{}
	}
	for _, owned := range NewFolder(c.ID).Secrets() {
		if _, ok := inputs[owned]; !ok {
			inputs[owned] = maskValue
		}
	}
	inputs[name] = value
	_, err = s.Patch(ctx, c.ID, ConnectionPatch{Inputs: &inputs})
	return err
}

// checkKey checks a saved key. A test command runs first. Else a protocol
// connection must reach connected after its restart. A connection with
// neither passes.
func (s *Service) checkKey(ctx context.Context, id string, d manifest.LoginMethod, before *proc) error {
	r, err := s.Resolve(id, "")
	if err != nil {
		return err
	}
	if d.Test != "" {
		out, err := procgroup.Command{Script: d.Test, Dir: r.WorkDir(), Env: s.processEnv(r)}.Run(ctx, keyTestOutput)
		if err != nil {
			if tail := lastLines(out, 3); tail != "" {
				return loginError(fmt.Sprintf("%s did not accept the key: %s", r.Title, tail))
			}
			return loginError(r.Title + " did not accept the key.")
		}
		return nil
	}
	if r.Manifest.Protocol() != manifest.ProtocolConnector {
		return nil
	}
	if p := s.procByName(id); p == nil || p == before && !p.alive() {
		if err := s.Start(ctx, id); err != nil {
			return err
		}
	}
	return s.awaitStatus(ctx, id, r.Title, before)
}

// awaitStatus waits for the first status of a process that is not before.
// connected and rate_limited pass; auth_required and error fail with the
// adapter message.
func (s *Service) awaitStatus(ctx context.Context, id, title string, before *proc) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if p := s.procByName(id); p != nil && p != before {
			st := p.authStatus()
			switch st.State {
			case protocol.StateConnected, protocol.StateRateLimited:
				return nil
			case protocol.StateAuthRequired, protocol.StateError:
				return loginError(cmp.Or(st.Message, title+" did not accept the key."))
			}
		}
		select {
		case <-ctx.Done():
			return loginError(title + " did not answer in time. Try again.")
		case <-tick.C:
		}
	}
}

// loginError is a login result that the Login tab shows as it is.
type loginError string

func (e loginError) Error() string { return string(e) }

// setKeyCheck stores the result of the last key check of a connection.
func (s *Service) setKeyCheck(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyChecks == nil {
		s.keyChecks = map[string]string{}
	}
	if err == nil {
		delete(s.keyChecks, id)
		return
	}
	s.keyChecks[id] = err.Error()
}

// loginRow is the login state of a connection row: {state, message}. The
// adapter state wins while the process runs. A connection with only key
// methods is auth_required while a key is empty or its last check failed.
// An OAuth login gives its own state. The state is "" for a connection with no login method.
func (s *Service) loginRow(r Resolved, env map[string]string, auth any) map[string]any {
	row := map[string]any{"state": "", "message": ""}
	if a, ok := auth.(map[string]any); ok && a != nil {
		row["state"], row["message"] = a["state"], a["message"]
		return row
	}
	if _, ok := s.oauthMethod(r); ok {
		return s.oauthRow(r.ID)
	}
	keys := r.Manifest.Setup.KeyInputs()
	if len(keys) == 0 {
		return row
	}
	row["state"] = protocol.StateConnected
	for _, name := range keys {
		if env[name] == "" {
			row["state"], row["message"] = protocol.StateAuthRequired, "Log in first."
			return row
		}
	}
	s.mu.Lock()
	msg, failed := s.keyChecks[r.ID]
	s.mu.Unlock()
	if failed {
		row["state"], row["message"] = protocol.StateAuthRequired, msg
	}
	return row
}
