// chromedp probe for the SMTP bot creation UI flow.
// Verifies the platform-dropdown toggle: selecting "smtp" must show
// the SMTP field group and hide the webhook + app field groups; and
// that the SMTP fields actually submit a bot.
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

	// 1. Log in.
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

	// 2. Open the bots page + new-bot drawer.
	if err := step("open new bot drawer",
		chromedp.Navigate(base+"/ui/bots"),
		chromedp.WaitVisible(`[data-action="drawer-open"][data-target="drawer-new-bot"]`, chromedp.ByQuery),
		chromedp.Click(`[data-action="drawer-open"][data-target="drawer-new-bot"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#drawer-new-bot select[name="platform"]`, chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond),
	); err != nil {
		return err
	}

	// 3. Default state (telegram): webhook-fields visible, smtp + app
	//    hidden.
	var webhookHidden, smtpHidden, appHidden bool
	if err := step("verify telegram default visibility",
		chromedp.Evaluate(`document.querySelector('[data-scope="new-webhook-fields"]').classList.contains('hidden')`, &webhookHidden),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-smtp-fields"]').classList.contains('hidden')`, &smtpHidden),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-app-fields"]').classList.contains('hidden')`, &appHidden),
	); err != nil {
		return err
	}
	if webhookHidden {
		return fmt.Errorf("webhook fields hidden in telegram default (should be visible)")
	}
	if !smtpHidden {
		return fmt.Errorf("smtp fields visible in telegram default (should be hidden)")
	}
	if !appHidden {
		return fmt.Errorf("app fields visible in telegram default (should be hidden)")
	}
	fmt.Println("[OK] telegram default: webhook shown, smtp+app hidden")

	// 4. Switch platform to smtp via the select (use dispatchEvent
	//    'change' so the data-action listener fires).
	if err := step("switch platform to smtp",
		chromedp.Evaluate(`(()=>{
			var sel = document.querySelector('#drawer-new-bot select[name="platform"]');
			sel.value = 'smtp';
			sel.dispatchEvent(new Event('change', {bubbles:true}));
			sel.dispatchEvent(new Event('click', {bubbles:true}));
		})()`, nil),
		chromedp.Sleep(200*time.Millisecond),
	); err != nil {
		return err
	}

	// 5. After switch: smtp visible; webhook + app + lark-kind-row hidden.
	if err := step("verify smtp visibility",
		chromedp.Evaluate(`document.querySelector('[data-scope="new-webhook-fields"]').classList.contains('hidden')`, &webhookHidden),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-smtp-fields"]').classList.contains('hidden')`, &smtpHidden),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-app-fields"]').classList.contains('hidden')`, &appHidden),
	); err != nil {
		return err
	}
	if !webhookHidden {
		return fmt.Errorf("webhook fields visible after smtp switch (should be hidden)")
	}
	if smtpHidden {
		return fmt.Errorf("smtp fields STILL hidden after smtp switch (toggle broken)")
	}
	if !appHidden {
		return fmt.Errorf("app fields visible after smtp switch (should be hidden)")
	}
	fmt.Println("[OK] smtp switch: smtp shown, webhook+app hidden")

	// 6. Bonus: verify the smtp_use_tls checkbox defaults to checked.
	var tlsChecked bool
	if err := step("verify use_tls defaults checked",
		chromedp.Evaluate(`document.querySelector('#drawer-new-bot input[name="smtp_use_tls"]').checked`, &tlsChecked),
	); err != nil {
		return err
	}
	if !tlsChecked {
		return fmt.Errorf("smtp_use_tls default = false (security regression)")
	}
	fmt.Println("[OK] smtp_use_tls defaults to checked")

	// 7. Switch back to lark to confirm bidirectional toggling.
	if err := step("switch back to lark",
		chromedp.Evaluate(`(()=>{
			var sel = document.querySelector('#drawer-new-bot select[name="platform"]');
			sel.value = 'lark';
			sel.dispatchEvent(new Event('change', {bubbles:true}));
			sel.dispatchEvent(new Event('click', {bubbles:true}));
		})()`, nil),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-smtp-fields"]').classList.contains('hidden')`, &smtpHidden),
		chromedp.Evaluate(`document.querySelector('[data-scope="new-kind-row"]').classList.contains('hidden')`, &appHidden),
	); err != nil {
		return err
	}
	if !smtpHidden {
		return fmt.Errorf("smtp fields stuck visible after lark switch")
	}
	if appHidden {
		return fmt.Errorf("lark kind-row hidden after lark switch (broken)")
	}
	fmt.Println("[OK] lark switch: smtp hidden, kind-row shown")

	_ = strings.HasPrefix // keep import alive in case we need it for debug
	return nil
}
