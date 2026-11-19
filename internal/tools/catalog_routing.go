package tools

import (
	"fmt"

	"github.com/trebi-ai/trebi/internal/request"
)

// routingActions are the connector actions that read and change the routes
// of one connection in one workspace.
func routingActions() map[string]Action {
	return map[string]Action{
		"routing_list": act("GET /v1/workspaces/{id}/routing", Input{"workspace": "ws1"}, func(in Input) (request.HTTPRequest, error) {
			ws, err := in.reqStr("workspace")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			return mcpGet(request.BuildRoutingListPath(ws)), nil
		}),
		"routing_get": act("GET /v1/workspaces/{id}/connectors/{ref}/routing", Input{"workspace": "ws1", "connector": "gmail"}, func(in Input) (request.HTTPRequest, error) {
			v, err := in.reqStrs("workspace", "connector")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			return request.BuildRoutingGet(v[0], v[1]), nil
		}),
		"routing_set": act("PATCH /v1/workspaces/{id}/connectors/{ref}/routing", Input{
			"workspace": "ws1", "connector": "gmail", "if_match": "\"h\"", "mode": "on", "notes": "Billing goes to ops.",
			"unsure": "ask", "no_fit": "drop", "sure_at": 0.7,
			"routes":        []any{map[string]any{"agent": "ops", "gets": "chosen", "use_for": "Billing questions"}},
			"remove_routes": []any{"rt_0123456789"},
			"rules":         []any{map[string]any{"sender_is": "a@b.co", "to": "ops"}},
		}, func(in Input) (request.HTTPRequest, error) {
			v, err := in.reqStrs("workspace", "connector")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			ifMatch, err := in.str("if_match")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			p, err := in.routingPatch()
			if err != nil {
				return request.HTTPRequest{}, err
			}
			return request.BuildRoutingSet(v[0], v[1], p, ifMatch), nil
		}),
	}
}

// routingPatch reads the patch fields of routing_set. An absent field stays
// nil, so the daemon leaves it alone.
func (in Input) routingPatch() (request.RoutingPatch, error) {
	var p request.RoutingPatch
	var err error
	for key, dst := range map[string]**string{"mode": &p.Mode, "notes": &p.Notes, "unsure": &p.Unsure, "no_fit": &p.NoFit} {
		if *dst, err = in.presentStr(key); err != nil {
			return p, err
		}
	}
	if v, ok := in["sure_at"]; ok && v != nil {
		f, isNum := v.(float64)
		if !isNum {
			return p, fmt.Errorf("input field %q must be a number", "sure_at")
		}
		p.SureAt = &f
	}
	if p.Routes, _, err = in.objList("routes"); err != nil {
		return p, err
	}
	if p.RemoveRoutes, err = in.strList("remove_routes"); err != nil {
		return p, err
	}
	if rules, ok, err := in.objList("rules"); err != nil {
		return p, err
	} else if ok {
		p.Rules = &rules
	}
	return p, nil
}

// objList reads one array-of-objects field. ok is false when it is absent.
func (in Input) objList(key string) (out []map[string]any, ok bool, err error) {
	v, exists := in[key]
	if !exists || v == nil {
		return nil, false, nil
	}
	arr, isArr := v.([]any)
	if !isArr {
		return nil, false, fmt.Errorf("input field %q must be an array of objects", key)
	}
	out = make([]map[string]any, len(arr))
	for i, e := range arr {
		m, isMap := e.(map[string]any)
		if !isMap {
			return nil, false, fmt.Errorf("input field %q must be an array of objects", key)
		}
		out[i] = m
	}
	return out, true, nil
}
