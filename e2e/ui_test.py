"""End-to-end UI test for the budget-guard page against the live proxy.

Run (from the repo root; CPA_BASE defaults to http://127.0.0.1:8317):
  docker run --rm --network host -e CPA_MGMT_KEY=... -v "$PWD/e2e:/e2e" \
    mcr.microsoft.com/playwright/python:v1.55.0-noble python3 -I /e2e/ui_test.py
Creates a key named "ui-test", edits it, and deletes it again (also from access.api-keys).
"""
import json
import os
import sys

from playwright.sync_api import sync_playwright, expect

BASE = os.environ.get("CPA_BASE", "http://127.0.0.1:8317")
KEY = os.environ["CPA_MGMT_KEY"]
PAGE = BASE + "/v0/resource/plugins/budget-guard/status"
SHOTS = "/e2e/shots"
os.makedirs(SHOTS, exist_ok=True)


def mgmt(page, path):
    r = page.request.get(BASE + path, headers={"Authorization": "Bearer " + KEY})
    assert r.ok, (path, r.status)
    return r.json()


def open_page(browser, theme):
    ctx = browser.new_context(viewport={"width": 1320, "height": 900}, device_scale_factor=1)
    ctx.add_init_script(
        "sessionStorage.setItem('budgetGuardKey', %s);"
        "localStorage.setItem('cli-proxy-theme', %s);"
        % (json.dumps(KEY), json.dumps(json.dumps({"state": {"theme": theme}})))
    )
    page = ctx.new_page()
    errors = []
    page.on("pageerror", lambda e: errors.append(str(e)))
    page.on("console", lambda m: m.type == "error" and errors.append(m.text))
    page.goto(PAGE)
    expect(page.locator(".meta")).to_contain_text("updated")
    page.wait_for_timeout(900)
    return ctx, page, errors


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch()

        for theme in ("light", "dark"):
            ctx, page, errors = open_page(browser, theme)
            page.screenshot(path=f"{SHOTS}/01-empty-{theme}.png", full_page=True)
            assert not errors, errors
            ctx.close()

        ctx, page, errors = open_page(browser, "light")
        # Create a key with Claude 5h own share 50% and pool ceiling 99%.
        page.get_by_role("button", name="Add key").first.click()
        page.locator("#kn").fill("ui-test")
        new_key = page.locator("#kk").input_value()
        assert new_key.startswith("sk-cpa-") and len(new_key) > 40, new_key
        pane = page.locator('[data-pane="claude"]')
        pane.locator('input[data-k="own"][data-w="5h"]').fill("50")
        pane.locator('input[data-k="ceil"][data-w="*"]').fill("99")
        page.locator('[data-tab="codex"]').click()
        page.locator('[data-pane="codex"] input[data-k="own"][data-w="7d"]').fill("40")
        page.screenshot(path=f"{SHOTS}/02-add-key-modal.png")
        page.locator("#ksave").click()
        expect(page.locator(".modal-title")).to_have_text("Connect ui-test")
        page.screenshot(path=f"{SHOTS}/03-connect-modal.png")
        page.get_by_role("button", name="Done").click()

        cfg = mgmt(page, "/v8/management/config/plugins/configs/budget-guard")
        keys = {k["name"]: k for k in cfg["keys"]}
        assert keys["ui-test"]["key"] == new_key, keys
        assert keys["ui-test"]["budgets"] == {
            "claude": {"own_share": {"5h": 50}, "pool_ceiling": {"*": 99}},
            "codex": {"own_share": {"7d": 40}},
        }, keys["ui-test"]["budgets"]
        assert new_key in mgmt(page, "/v8/management/config/access/api-keys")
        print("create: config + api-keys OK")

        expect(page.locator(".card-title", has_text="ui-test")).to_be_visible()
        page.wait_for_timeout(600)
        page.screenshot(path=f"{SHOTS}/04-key-card-light.png", full_page=True)

        # Rename and drop the Codex limit.
        page.locator('[data-act="edit"]').first.click()
        page.locator("#kn").fill("ui-test-renamed")
        page.locator('[data-tab="codex"]').click()
        page.locator('[data-pane="codex"] input[data-k="own"][data-w="7d"]').fill("")
        page.locator("#ksave").click()
        expect(page.locator(".card-title", has_text="ui-test-renamed")).to_be_visible()
        cfg = mgmt(page, "/v8/management/config/plugins/configs/budget-guard")
        k = cfg["keys"][0]
        assert k["name"] == "ui-test-renamed" and "codex" not in k["budgets"] and k["key"] == new_key, k
        print("edit: OK")

        # Settings modal renders and saves unchanged values.
        page.get_by_role("button", name="Settings").click()
        page.locator("#padd").click()
        page.screenshot(path=f"{SHOTS}/05-settings.png")
        page.locator("button[data-rm]").last.click()
        page.locator("#ssave").click()
        expect(page.locator(".toast").last).to_contain_text("Settings saved")
        print("settings: OK")
        ctx.close()

        ctx, page, errors = open_page(browser, "dark")
        page.screenshot(path=f"{SHOTS}/06-key-card-dark.png", full_page=True)
        page.set_viewport_size({"width": 400, "height": 860})
        page.wait_for_timeout(300)
        page.screenshot(path=f"{SHOTS}/07-mobile-dark.png", full_page=True)
        page.set_viewport_size({"width": 1320, "height": 900})

        # Delete: removed from budgets and from access.api-keys.
        page.locator('[data-act="delete"]').first.click()
        page.locator("#del").click()
        expect(page.locator(".empty-title")).to_be_visible()
        cfg = mgmt(page, "/v8/management/config/plugins/configs/budget-guard")
        assert not cfg.get("keys"), cfg
        assert new_key not in mgmt(page, "/v8/management/config/access/api-keys")
        print("delete: config + api-keys OK")
        assert not errors, errors
        ctx.close()
        browser.close()
    print("ALL OK")


if __name__ == "__main__":
    try:
        main()
    except AssertionError as e:
        print("FAIL:", e)
        sys.exit(1)
