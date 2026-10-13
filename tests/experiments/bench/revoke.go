package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// runRevoke measures how long a live terminal survives after its permission
// is withdrawn: device revoked, member downgraded to viewer, member removed,
// web session logged out. Each trial uses a fresh virtual device so revoking
// does not touch the real one; the Hub path is the same.
func runRevoke(s Stack, flags *flag.FlagSet, args []string) error {
	n := flags.Int("n", 30, "trials per kind")
	_ = flags.Parse(args)

	owner, err := login(s.Hub, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	space := owner.spaces[0].ID
	out, err := newTable(s, "revoke.csv", "kind", "trial", "closed", "close_code", "effect_ms", "reconnect_status")
	if err != nil {
		return err
	}
	defer out.close()

	devices, err := createSimDevices(owner, space, "exp-revoke", *n*4, 4)
	if err != nil {
		return err
	}
	if err := connectAll(devices, s.Hub, 8); err != nil {
		return err
	}
	defer func() {
		for _, d := range devices {
			d.close()
		}
	}()
	ids := make([]string, len(devices))
	for i, d := range devices {
		ids[i] = d.ID
	}
	if err := waitOnline(owner, ids, 30*time.Second); err != nil {
		return err
	}

	newMember := func(i int) (*client, string, error) {
		var created struct {
			User  struct{ ID string }
			Token string
		}
		status, err := owner.call(http.MethodPost, "/api/v1/spaces/"+space+"/members", map[string]string{"name": fmt.Sprintf("Exp Rev %d", i), "role": "operator"}, &created)
		if err != nil || status != http.StatusCreated {
			return nil, "", fmt.Errorf("add member: HTTP %d %v", status, err)
		}
		c, err := login(s.Hub, s.Proxy, created.Token)
		return c, created.User.ID, err
	}

	// Login is limited to 10 per minute per address, so the downgrade trials
	// share one member whose role is restored after each trial.
	shared, sharedID, err := newMember(-1)
	if err != nil {
		return err
	}
	trial := func(kind string, i int, d *simDevice) error {
		user, userID := owner, ""
		switch kind {
		case "downgrade":
			user, userID = shared, sharedID
			defer owner.call(http.MethodPatch, "/api/v1/spaces/"+space+"/members/"+sharedID, map[string]string{"role": "operator"}, nil)
		case "remove", "logout":
			var err error
			if user, userID, err = newMember(i); err != nil {
				return err
			}
		}
		t, _, err := openTerminal(user, d.ID, "s", 10*time.Second)
		if err != nil {
			return fmt.Errorf("%s trial %d: %w", kind, i, err)
		}
		defer t.close()
		// Random phase against the 2 s permission timer.
		time.Sleep(time.Duration(300+i*137%1700) * time.Millisecond)
		start := time.Now()
		var status int
		switch kind {
		case "device":
			status, err = owner.call(http.MethodDelete, "/api/v1/spaces/"+space+"/devices/"+d.ID, nil, nil)
		case "downgrade":
			status, err = owner.call(http.MethodPatch, "/api/v1/spaces/"+space+"/members/"+userID, map[string]string{"role": "viewer"}, nil)
		case "remove":
			status, err = owner.call(http.MethodDelete, "/api/v1/spaces/"+space+"/members/"+userID, nil, nil)
		case "logout":
			status, err = user.call(http.MethodPost, "/api/v1/auth/logout", nil, nil)
		}
		if err != nil || status >= 300 {
			return fmt.Errorf("%s trial %d: HTTP %d %v", kind, i, status, err)
		}
		select {
		case <-t.done:
		case <-time.After(10 * time.Second):
		}
		reconnect := ""
		if kind == "device" {
			// The revoked device must not be able to come back.
			d.close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, response, dialErr := websocket.Dial(ctx, d.hubWS, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + d.Token}, "X-Sinthmux-Device-Id": {d.ID}}, HTTPClient: &http.Client{Transport: &http.Transport{Proxy: nil}}})
			cancel()
			if dialErr == nil {
				reconnect = "101"
			} else if response != nil {
				reconnect = fmt.Sprint(response.StatusCode)
			}
		}
		if t.closedAt.IsZero() {
			out.row(kind, i, 0, "", "", reconnect)
		} else {
			out.row(kind, i, 1, int(t.closeCode), t.closedAt.Sub(start), reconnect)
		}
		return nil
	}
	index := 0
	for _, kind := range []string{"device", "downgrade", "remove", "logout"} {
		for i := 0; i < *n; i++ {
			if err := trial(kind, i, devices[index]); err != nil {
				return err
			}
			index++
		}
		logf("revoke %s done", kind)
	}
	return nil
}
