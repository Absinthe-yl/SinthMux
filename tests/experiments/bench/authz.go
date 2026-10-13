package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// restartConnector kills the real connector; the supervisor in run.sh starts
// it again after one second. It waits until the device is online again.
func restartConnector(s Stack, owner *client, deviceID string) error {
	_ = exec.Command("pkill", "-f", s.ConnBin+"$").Run()
	time.Sleep(1500 * time.Millisecond)
	owner.http.CloseIdleConnections()
	_, err := owner.clone(s.Hub, 5*time.Second).deviceByName(s.Device, 30*time.Second)
	return err
}

// ---------- unauthorized request interception ----------

type authzCase struct {
	group, name, actor string
	method, path       string
	body               any
	raw                func() (int, error) // custom request instead of method/path
	allowed            []int               // statuses that count as intercepted
}

func runAuthz(s Stack, flags *flag.FlagSet, args []string) error {
	repeats := flags.Int("n", 20, "repetitions per case")
	_ = flags.Parse(args)

	owner, err := login(s.Hub, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	device, err := owner.deviceByName(s.Device, 20*time.Second)
	if err != nil {
		return err
	}
	space := owner.spaces[0].ID
	_ = owner.ensureSession(device.ID, "exp-authz")

	// Members with each role in the owner's space, plus an outsider with an own space.
	member := func(name, role string) (*client, string, error) {
		var out struct {
			User  struct{ ID string }
			Token string
		}
		status, err := owner.call(http.MethodPost, "/api/v1/spaces/"+space+"/members", map[string]string{"name": name, "role": role}, &out)
		if err != nil || status != http.StatusCreated {
			return nil, "", fmt.Errorf("add %s: HTTP %d %v", role, status, err)
		}
		c, err := login(s.Hub, s.Proxy, out.Token)
		return c, out.User.ID, err
	}
	viewer, viewerID, err := member("Exp Viewer", "viewer")
	if err != nil {
		return err
	}
	operator, operatorID, err := member("Exp Operator", "operator")
	if err != nil {
		return err
	}
	admin, _, err := member("Exp Admin", "admin")
	if err != nil {
		return err
	}
	// The outsider is a member of a second space only.
	var second struct{ ID string }
	if status, err := owner.call(http.MethodPost, "/api/v1/spaces", map[string]string{"name": "Exp Other"}, &second); err != nil || status != http.StatusCreated {
		return fmt.Errorf("create space: HTTP %d %v", status, err)
	}
	var outsiderOut struct{ Token string }
	if status, err := owner.call(http.MethodPost, "/api/v1/spaces/"+second.ID+"/members", map[string]string{"name": "Exp Outsider", "role": "admin"}, &outsiderOut); err != nil || status != http.StatusCreated {
		return fmt.Errorf("add outsider: HTTP %d %v", status, err)
	}
	outsider, err := login(s.Hub, s.Proxy, outsiderOut.Token)
	if err != nil {
		return err
	}
	anonymous := newClient(s.Hub, s.Proxy)
	actors := map[string]*client{"viewer": viewer, "operator": operator, "admin": admin, "outsider": outsider, "anonymous": anonymous}

	dev := "/api/v1/devices/" + url.PathEscape(device.ID)
	sess := dev + "/sessions/exp-authz"
	spaceP := "/api/v1/spaces/" + space
	denied := []int{401, 403, 404}

	// A valid session cookie with a missing or wrong CSRF token, and a foreign Origin.
	noCSRF := func() (int, error) {
		c := *owner
		c.csrf = ""
		return c.call(http.MethodPost, dev+"/sessions", map[string]string{"name": "exp-csrf"}, nil)
	}
	badOrigin := func() (int, error) {
		c := *owner
		c.origin = "https://evil.example"
		return c.call(http.MethodPost, dev+"/sessions", map[string]string{"name": "exp-origin"}, nil)
	}
	wsWith := func(c *client, ticketOf func() string, origin string) func() (int, error) {
		return func() (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			d := *c
			if origin != "" {
				d.origin = origin
			}
			t, response, err := dialTerminal(ctx, &d, ticketOf())
			if err == nil {
				t.close()
				return 101, nil
			}
			if response != nil {
				return response.StatusCode, nil
			}
			return 0, err
		}
	}
	freshTicket := func() string {
		ticket, _, _ := owner.ticket(context.Background(), device.ID, "exp-authz")
		return ticket
	}
	usedTicket := func() string {
		ticket := freshTicket()
		if t, _, err := dialTerminal(context.Background(), owner, ticket); err == nil {
			t.close()
		}
		return ticket
	}
	connectorForge := func(header http.Header) func() (int, error) {
		return func() (int, error) {
			request, _ := http.NewRequest(http.MethodGet, s.Hub+"/ws/v1/connectors/connect", nil)
			for k, v := range header {
				request.Header[k] = v
			}
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			response, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(request)
			if err != nil {
				return 0, err
			}
			response.Body.Close()
			return response.StatusCode, nil
		}
	}
	upload := func(c *client) func() (int, error) {
		return func() (int, error) {
			status, _, err := c.request(context.Background(), http.MethodPost, dev+"/uploads?name=x.txt", "application/octet-stream", bytes.NewReader([]byte("x")))
			return status, err
		}
	}

	var cases []authzCase
	add := func(group, name, actor, method, path string, body any) {
		cases = append(cases, authzCase{group: group, name: name, actor: actor, method: method, path: path, body: body, allowed: denied})
	}
	// Role matrix: every action a role must not perform.
	for _, actor := range []string{"viewer"} {
		add("role", "终端票据（输入）", actor, "POST", sess+"/ticket", nil)
		add("role", "创建会话", actor, "POST", dev+"/sessions", map[string]string{"name": "exp-x"})
		add("role", "重命名会话", actor, "PATCH", sess, map[string]string{"name": "exp-y"})
		add("role", "导出历史", actor, "GET", sess+"/scrollback", nil)
		cases = append(cases, authzCase{group: "role", name: "上传文件", actor: actor, raw: upload(viewer), allowed: denied})
	}
	for _, actor := range []string{"viewer", "operator"} {
		add("role", "关闭会话", actor, "DELETE", dev+"/sessions/exp-nonexistent", nil)
		add("role", "查看审计", actor, "GET", spaceP+"/audit", nil)
		add("role", "生成配对码", actor, "POST", spaceP+"/device-pairings", map[string]string{"name": "x"})
		add("role", "吊销设备", actor, "DELETE", spaceP+"/devices/"+device.ID, nil)
	}
	for _, actor := range []string{"viewer", "operator", "admin"} {
		add("role", "添加成员", actor, "POST", spaceP+"/members", map[string]string{"name": "x", "role": "viewer"})
		add("role", "修改角色", actor, "PATCH", spaceP+"/members/"+viewerID, map[string]string{"role": "admin"})
		add("role", "移除成员", actor, "DELETE", spaceP+"/members/"+operatorID, nil)
		add("role", "删除空间", actor, "DELETE", spaceP, nil)
	}
	// Cross-space access by a user of another space.
	add("space", "列出他人设备会话", "outsider", "GET", dev+"/sessions", nil)
	add("space", "他人设备终端票据", "outsider", "POST", sess+"/ticket", nil)
	add("space", "他人空间审计", "outsider", "GET", spaceP+"/audit", nil)
	add("space", "他人空间成员", "outsider", "GET", spaceP+"/members", nil)
	add("space", "吊销他人设备", "outsider", "DELETE", spaceP+"/devices/"+device.ID, nil)
	// No session at all.
	add("anon", "设备列表", "anonymous", "GET", "/api/v1/devices", nil)
	add("anon", "终端票据", "anonymous", "POST", sess+"/ticket", nil)
	add("anon", "空间列表", "anonymous", "GET", "/api/v1/spaces", nil)
	// Request forgery and credential misuse.
	cases = append(cases,
		authzCase{group: "forgery", name: "缺少 CSRF 令牌", actor: "owner", raw: noCSRF, allowed: []int{403}},
		authzCase{group: "forgery", name: "跨站 Origin", actor: "owner", raw: badOrigin, allowed: []int{403}},
		authzCase{group: "ticket", name: "重放已用票据", actor: "owner", raw: wsWith(owner, usedTicket, ""), allowed: []int{401}},
		authzCase{group: "ticket", name: "伪造票据", actor: "owner", raw: wsWith(owner, func() string { return strings.Repeat("ab", 32) }, ""), allowed: []int{401}},
		authzCase{group: "ticket", name: "他人会话用票据", actor: "operator", raw: wsWith(operator, freshTicket, ""), allowed: []int{403}},
		authzCase{group: "ticket", name: "无 Cookie 用票据", actor: "anonymous", raw: wsWith(anonymous, freshTicket, ""), allowed: []int{401}},
		authzCase{group: "ticket", name: "跨站 Origin 终端", actor: "owner", raw: wsWith(owner, freshTicket, "https://evil.example"), allowed: []int{403}},
		authzCase{group: "device", name: "无凭据接入设备通道", actor: "anonymous", raw: connectorForge(http.Header{}), allowed: []int{401}},
		authzCase{group: "device", name: "伪造设备令牌", actor: "anonymous", raw: connectorForge(http.Header{"Authorization": {"Bearer smd_" + device.ID + "_" + strings.Repeat("0", 64)}, "X-Sinthmux-Device-Id": {device.ID}}), allowed: []int{401}},
		authzCase{group: "device", name: "伪造设备证书签名", actor: "anonymous", raw: connectorForge(http.Header{"X-Sinthmux-Device-Id": {device.ID}, "X-Sinthmux-Device-Nonce": {"x"}, "X-Sinthmux-Device-Certificate": {"AAAA"}, "X-Sinthmux-Device-Proof": {"AAAA"}}), allowed: []int{401}},
	)

	out, err := newTable(s, "authz.csv", "group", "case", "actor", "trial", "status", "intercepted")
	if err != nil {
		return err
	}
	defer out.close()
	total, blocked := 0, 0
	for _, c := range cases {
		for i := 0; i < *repeats; i++ {
			var status int
			var err error
			if c.raw != nil {
				status, err = c.raw()
			} else {
				status, err = actors[c.actor].call(c.method, c.path, c.body, nil)
			}
			ok := err == nil && contains(c.allowed, status)
			out.row(c.group, c.name, c.actor, i, status, b2i(ok))
			total++
			if ok {
				blocked++
			}
		}
	}
	// Positive control: allowed requests must still succeed, otherwise "100 %
	// intercepted" could just mean the stack is broken.
	controls, err := newTable(s, "authz_control.csv", "case", "actor", "status", "ok")
	if err != nil {
		return err
	}
	defer controls.close()
	check := func(name, actor string, status int, err error, want ...int) {
		controls.row(name, actor, status, b2i(err == nil && contains(want, status)))
	}
	status, err := operator.call("POST", sess+"/ticket", nil, nil)
	check("终端票据", "operator", status, err, 200)
	status, err = viewer.call("GET", dev+"/sessions", nil, nil)
	check("列出会话", "viewer", status, err, 200)
	status, err = admin.call("GET", spaceP+"/audit", nil, nil)
	check("查看审计", "admin", status, err, 200)
	status, err = owner.call("GET", spaceP+"/members", nil, nil)
	check("成员列表", "owner", status, err, 200)
	status, err = wsWith(operator, func() string { t, _, _ := operator.ticket(context.Background(), device.ID, "exp-authz"); return t }, "")()
	check("终端 WebSocket", "operator", status, err, 101)
	logf("authz: %d/%d intercepted over %d cases", blocked, total, len(cases))
	return nil
}

func contains(values []int, v int) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func b2i(ok bool) int {
	if ok {
		return 1
	}
	return 0
}
