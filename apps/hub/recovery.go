package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sinthmux/sinthmux/internal/auth"
)

// recoveryToken implements `sinthmux-hub recovery-token [user-id]`, run by the
// Hub operator when a user has lost every login token. Without an ID it lists
// users; with one it prints a 24-hour token for that user. Existing tokens,
// sessions and devices are left untouched.
func recoveryToken(store *auth.Store, args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	users, err := store.Users(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法读取用户：", err)
		return 1
	}
	userID := ""
	if len(args) > 0 {
		userID = args[0]
	} else if len(users) == 1 {
		userID = users[0]["id"]
	}
	if userID == "" {
		fmt.Fprintln(os.Stderr, "请指定要恢复的用户 ID：")
		for _, user := range users {
			github := ""
			if user["github"] != "" {
				github = "  GitHub " + user["github"]
			}
			fmt.Fprintf(os.Stderr, "  %s  %s%s  空间：%s\n", user["id"], user["name"], github, user["spaces"])
		}
		return 2
	}
	token, err := store.RecoveryToken(ctx, userID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "找不到该用户或用户已停用：", userID)
		return 1
	}
	store.Audit(ctx, "", "", userID, "auth.recovery_token", "ok")
	fmt.Println(token)
	return 0
}
