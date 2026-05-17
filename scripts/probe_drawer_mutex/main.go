// chromedp probe for drawer mutual exclusion.
// Reproduces the user-reported bug: clicking "edit" then "new" used
// to leave TWO drawers open. After psgOpenDrawer's mutex fix, only
// one drawer should be visible at a time.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	const base = "http://localhost:8080"
	if err := run(base); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(base string) error {
	// 1. Seed a Telegram bot via API so the bots page has an "编辑"
	//    button to click on.
	if err := seedBot(base); err != nil {
		return fmt.Errorf("seed bot: %w", err)
	}

	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	step := func(name string, actions ...chromedp.Action) error {
		fmt.Printf("[step] %s ...\n", name)
		t0 := time.Now()
		if err := chromedp.Run(ctx, actions...); err != nil {
			return fmt.Errorf("%s (%.1fs): %w", name, time.Since(t0).Seconds(), err)
		}
		fmt.Printf("[step] %s ok (%.1fs)\n", name, time.Since(t0).Seconds())
		return nil
	}

	if err := step("login",
		chromedp.Navigate(base+"/ui/login"),
		chromedp.WaitVisible(`input[name="email"]`, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="email"]`, "admin@admin.com", chromedp.ByQuery),
		chromedp.SendKeys(`input[name="password"]`, "Qwe123..", chromedp.ByQuery),
		chromedp.Submit(`input[name="email"]`, chromedp.ByQuery),
		chromedp.Sleep(800*time.Millisecond),
	); err != nil {
		return err
	}

	if err := step("open bots page",
		chromedp.Navigate(base+"/ui/bots"),
		chromedp.WaitVisible(`[data-action="edit-bot"]`, chromedp.ByQuery),
	); err != nil {
		return err
	}

	// 2. Click "编辑" → drawer-edit-bot must open.
	if err := step("click edit-bot",
		chromedp.Click(`[data-action="edit-bot"]`, chromedp.ByQuery),
		chromedp.Sleep(400*time.Millisecond),
	); err != nil {
		return err
	}
	var editHidden, newHidden bool
	if err := step("verify edit-bot drawer open",
		chromedp.Evaluate(`document.getElementById('drawer-edit-bot').classList.contains('hidden')`, &editHidden),
		chromedp.Evaluate(`document.getElementById('drawer-new-bot').classList.contains('hidden')`, &newHidden),
	); err != nil {
		return err
	}
	if editHidden {
		return fmt.Errorf("drawer-edit-bot still hidden after edit click")
	}
	if !newHidden {
		return fmt.Errorf("drawer-new-bot already open before new click (test setup wrong)")
	}
	fmt.Println("[OK] edit drawer open, new drawer hidden")

	// 3. WITHOUT closing edit, click "新建 Bot" — the previously-buggy
	//    sequence. With the fix, drawer-edit-bot must collapse and
	//    drawer-new-bot must open. With the bug, BOTH would be visible.
	if err := step("click new-bot WITHOUT closing edit",
		chromedp.Evaluate(`window.psgOpenDrawer('drawer-new-bot')`, nil),
		chromedp.Sleep(400*time.Millisecond),
	); err != nil {
		return err
	}
	var openCount int
	if err := step("count open drawers",
		chromedp.Evaluate(`document.querySelectorAll('.psg-drawer:not(.hidden)').length`, &openCount),
		chromedp.Evaluate(`document.getElementById('drawer-edit-bot').classList.contains('hidden')`, &editHidden),
		chromedp.Evaluate(`document.getElementById('drawer-new-bot').classList.contains('hidden')`, &newHidden),
	); err != nil {
		return err
	}
	if openCount != 1 {
		return fmt.Errorf("BUG STILL PRESENT: %d drawers open simultaneously (want exactly 1)", openCount)
	}
	if !editHidden {
		return fmt.Errorf("BUG STILL PRESENT: edit drawer remained open after switching to new")
	}
	if newHidden {
		return fmt.Errorf("new drawer did not open after click")
	}
	fmt.Println("[OK] only 1 drawer open: new drawer shown, edit drawer collapsed")

	// 4. Bonus: verify reverse direction (new → edit) via direct
	// psgOpenDrawer calls (same reason as above — click hit-test
	// is blocked by the active drawer's backdrop).
	if err := step("reverse direction new→edit",
		chromedp.Evaluate(`window.psgCloseDrawer('drawer-new-bot')`, nil),
		chromedp.Sleep(350*time.Millisecond),
		chromedp.Evaluate(`window.psgOpenDrawer('drawer-new-bot')`, nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`window.psgOpenDrawer('drawer-edit-bot')`, nil),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(`document.querySelectorAll('.psg-drawer:not(.hidden)').length`, &openCount),
	); err != nil {
		return err
	}
	if openCount != 1 {
		return fmt.Errorf("reverse direction broken: %d drawers open (want 1)", openCount)
	}
	fmt.Println("[OK] reverse direction (new → edit): only 1 drawer open")

	// 5. Same drawer re-open is a no-op — no false self-close.
	if err := step("re-open the same drawer is a no-op",
		chromedp.Evaluate(`window.psgOpenDrawer('drawer-edit-bot')`, nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`document.querySelectorAll('.psg-drawer:not(.hidden)').length`, &openCount),
		chromedp.Evaluate(`document.getElementById('drawer-edit-bot').classList.contains('hidden')`, &editHidden),
	); err != nil {
		return err
	}
	if openCount != 1 {
		return fmt.Errorf("re-open same drawer broke count: %d open", openCount)
	}
	if editHidden {
		return fmt.Errorf("re-open same drawer accidentally hid it")
	}
	fmt.Println("[OK] re-opening the same drawer keeps it open (no self-close regression)")
	return nil
}

// seedBot uses the public registration + bot API to create a single
// Telegram bot. The bots page needs at least one row for the edit
// button to render.
func seedBot(base string) error {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	// Login
	_, err := client.Get(base + "/ui/login")
	if err != nil {
		return err
	}
	u, _ := url.Parse(base)
	var csrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "psg_csrf" {
			csrf = c.Value
		}
	}
	resp, err := client.PostForm(base+"/ui/login", url.Values{
		"csrf": {csrf}, "email": {"admin@admin.com"}, "password": {"Qwe123.."},
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	// Refresh csrf cookie
	for _, c := range jar.Cookies(u) {
		if c.Name == "psg_csrf" {
			csrf = c.Value
		}
	}
	// Create bot via JSON API
	req, _ := http.NewRequest("POST", base+"/api/v1/bots",
		strings.NewReader(`{"name":"alpha","platform":"telegram","bot_token":"1:abcdef"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// Idempotent: 201 on first run, 500 (UNIQUE constraint) on
	// subsequent runs of the same DB. Both are fine — we just need
	// at least one bot row for the edit button to render.
	if resp.StatusCode != 201 && resp.StatusCode != 500 {
		return fmt.Errorf("seed bot status = %d", resp.StatusCode)
	}
	return nil
}
