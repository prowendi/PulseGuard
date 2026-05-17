// chromedp probe for the templates page "复制到编辑器" workflow.
// Verifies the fix end-to-end. Logs in inside the browser to avoid
// having to inject cookies via the network API.
package main

import (
	"context"
	"fmt"
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
	ctx, cancel := chromedp.NewContext(context.Background(),
		chromedp.WithLogf(func(s string, args ...interface{}) {
			fmt.Printf("[chromedp] "+s+"\n", args...)
		}),
	)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Run actions individually so a failure pinpoints the step.
	step := func(name string, actions ...chromedp.Action) error {
		fmt.Printf("[step] %s ...\n", name)
		t0 := time.Now()
		if err := chromedp.Run(ctx, actions...); err != nil {
			return fmt.Errorf("%s (%.1fs): %w", name, time.Since(t0).Seconds(), err)
		}
		fmt.Printf("[step] %s ok (%.1fs)\n", name, time.Since(t0).Seconds())
		return nil
	}

	if err := step("navigate to login",
		chromedp.Navigate(base+"/ui/login"),
		chromedp.WaitVisible(`input[name="email"]`, chromedp.ByQuery),
	); err != nil {
		return err
	}
	if err := step("submit credentials",
		chromedp.SendKeys(`input[name="email"]`, "admin@admin.com", chromedp.ByQuery),
		chromedp.SendKeys(`input[name="password"]`, "Qwe123..", chromedp.ByQuery),
		chromedp.Submit(`input[name="email"]`, chromedp.ByQuery),
		chromedp.Sleep(800*time.Millisecond),
	); err != nil {
		return err
	}
	if err := step("navigate to /ui/templates",
		chromedp.Navigate(base+"/ui/templates"),
		chromedp.WaitVisible(`[data-tpl-demo="basic-alert"]`, chromedp.ByQuery),
	); err != nil {
		return err
	}

	var bodyValAfterCopy, nameValAfterCopy, modeValAfterCopy string
	var bodyValAfterBold string
	if err := step("click demo + read fields",
		chromedp.Click(`[data-tpl-demo="basic-alert"]`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Value(`#tplBody`, &bodyValAfterCopy, chromedp.ByQuery),
		chromedp.Value(`#drawer-new-tpl input[name="name"]`, &nameValAfterCopy, chromedp.ByQuery),
		chromedp.Value(`#drawer-new-tpl select[name="parse_mode"]`, &modeValAfterCopy, chromedp.ByQuery),
	); err != nil {
		return err
	}
	if err := step("click Bold + read body",
		chromedp.Click(`#drawer-new-tpl button[data-tpl-insert="bold"]`, chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Value(`#tplBody`, &bodyValAfterBold, chromedp.ByQuery),
	); err != nil {
		return err
	}

	if bodyValAfterCopy == "" {
		return fmt.Errorf("BUG-1 still present: textarea empty after demo click")
	}
	if !strings.Contains(bodyValAfterCopy, "{{ .title | escMD }}") {
		return fmt.Errorf("textarea content does not look like basic-alert demo: %q", bodyValAfterCopy)
	}
	fmt.Println("[OK] demo body copied:", firstLine(bodyValAfterCopy))

	if nameValAfterCopy != "basic-alert" {
		return fmt.Errorf("name input = %q, want basic-alert", nameValAfterCopy)
	}
	fmt.Println("[OK] name auto-filled:", nameValAfterCopy)

	if modeValAfterCopy != "MarkdownV2" {
		return fmt.Errorf("parse_mode = %q, want MarkdownV2", modeValAfterCopy)
	}
	fmt.Println("[OK] parse_mode auto-set:", modeValAfterCopy)

	if !strings.Contains(bodyValAfterBold, "*粗体文字*") {
		return fmt.Errorf("BUG-2 still present: bold button did not insert *粗体文字* (body=%q)", firstLine(bodyValAfterBold))
	}
	fmt.Println("[OK] toolbar Bold inserted *粗体文字* into the open drawer")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}
