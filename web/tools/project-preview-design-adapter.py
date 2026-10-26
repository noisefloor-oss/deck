#!/usr/bin/env python3
"""Prepare source-derived project preview references for noise visual-diff."""

import argparse
import base64
import hashlib
import json
import re
import zipfile
from pathlib import Path


SOURCE_SHA256 = "453ed450ae984861d9a375ebca6a37329f6956ee30e93051edb85e8af0a79214"
SOURCE_COMP = "Project Preview Flow.dc.html"
SOURCE_COMP_SHA256 = "9677ceb3370fd1c5fabcc54153066060cbf16b741de0fb091e13c7ab1f31104b"
MENU_COMP = "Workspace 6 Desktop Polish.dc.html"
MENU_COMP_SHA256 = "4f84aae9de0bac1d7cb313428645d2edd5b965b437c5ca2ef11032823553cf70"
CHECKED_WHEN = "just now"
DS_PREFIX = "_ds/blueprint-ac1da416-122f-4dbd-8421-498543a11fe7/"


def digest(body):
    return hashlib.sha256(body).hexdigest()


def replace(text, before, after, reason, changes, count=1):
    found = text.count(before)
    if found != count:
        raise ValueError(f"{reason}: expected {count} source occurrences, found {found}")
    changes.append({"reason": reason, "before": before, "after": after, "count": count})
    return text.replace(before, after)


def corrected_canvas(canvas):
    changes = []
    corrections = [
        ("Previews aren't set up on atlas-7", "Previews aren't set up", "The API does not identify an installation by the fictional atlas-7 name."),
        ("The installation owner sets a preview domain and ingress once.", "The installation owner enables previews once.", "Previews also run from loopback addresses, with no preview domain or ingress."),
        ("Only people signed in to atlas-7 can open it.", "Only people signed in to this Deck installation can open it.", "Describe the actual authentication boundary."),
        ("only people signed in to atlas-7 can open the URL.", "only people signed in to this Deck installation can open the URL.", "Describe the actual authentication boundary."),
        ("Deck doesn't read the project or pick a framework.", "Deck doesn't pick a framework.", "The declared launcher configuration is project data."),
        ("Reserving a URL and port…", "Assigning a URL and port…", "A registration reserves an assignment, not an operating-system socket."),
        ("Couldn't enable the preview", "Enable outcome unconfirmed", "A missing response does not prove the operation failed."),
        ("The preview service didn't answer. Nothing was reserved.", "The request did not complete successfully. A URL and port may already be assigned. Refresh settings to confirm the saved choice.", "A missing response does not prove no registration was written."),
        ("Every port in this installation's preview range is reserved. Disable a preview you no longer use, or ask the installation owner to widen the range.", "Ask the installation owner to expand the preview port range. Disabling a preview keeps its reserved port.", "Disabled registrations retain their URL and port."),
        ("The URL stops working for everyone signed in, including you. The port is released.", "Browser access is revoked for everyone signed in, including you. The URL and port stay reserved.", "Disabling revokes browser access without releasing the assignment."),
        ("The dev server isn't stopped. If one is running, stop it from the project's session. You can enable a preview again later; it may get a different URL.", "The dev server isn't stopped. If one is running, stop it from the project's session. Enabling this preview again uses the same URL and port.", "Disabling neither stops the process nor changes the assignment."),
        ("Couldn't disable access", "Disable outcome unconfirmed", "An unsuccessful response may follow runtime revocation."),
        ("The preview service didn't answer. The URL still works.", "The request did not complete successfully. Browser access may already be revoked, even if the saved settings still say enabled. Retry disabling to confirm it is saved.", "The registry may revoke the runtime route before a persistence failure."),
        ("rewrite-scheduler no longer has a preview URL. Its menu shows Preview… again.", "rewrite-scheduler keeps its URL and reserved port. Its menu shows Preview… again.", "Disabled registrations retain their assignment."),
        ("rewrite-scheduler has a stable private URL. Nothing is running yet — the dev server starts from the project\\'s session.", "rewrite-scheduler has a stable private URL. The dev server starts from the project\\'s session; connectivity hasn\\'t been checked.", "Enabling does not observe the server process."),
        ("If that's you, the steps are under Previews in the installation guide. Nothing here changes until it's done.", "The installation owner can enable self-service previews using the project preview operations guide.", "Point to the available generic administrator instructions."),
        ('Start the dev server from the project\'s session; the agent launches it through Deck and it picks up this assignment. Opening the URL before then shows Deck\'s "nothing is listening" page.', "Start the dev server from the project’s session. The agent launches the declared command through Deck and the launcher supplies this assignment.", "The gateway does not promise the prototype's fictional error page."),
        ("copyUrlLabel: st.copiedUrl ? 'Copied' : 'Copy',", "copyUrlLabel: st.copiedUrl ? 'Copied' : 'Copy URL',", "Make the clipboard action explicit."),
        ("statusText: st.checking ? 'Checking connectivity…' : (up ? 'Accepting connections' : 'Not responding'),", "statusText: st.checking ? 'Checking…' : (st.checkFailed ? 'Check failed' : (!st.hasChecked ? 'Not checked yet' : (up ? 'Accepting connections' : 'Not responding'))),", "Unknown, pending and failed checks must not claim a connectivity observation."),
        ("statusShort: up ? 'Accepting connections' : 'Not responding',", "statusShort: st.hasChecked ? (up ? 'Accepting connections' : 'Not responding') : 'Not checked yet',", "A menu registration does not establish reachability."),
        ("statusWhen: st.checking ? '' : '· checked ' + st.checkedWhen,", "statusWhen: st.checking || st.checkFailed || !st.hasChecked ? '' : '· checked ' + st.checkedWhen,", "Show an observation age only when the fixture has an actual check result."),
        ("lastCheck: st.checking ? 'In progress' : (up ? 'Connection accepted · ' + st.checkedWhen : 'Connection refused · ' + st.checkedWhen),", "lastCheck: st.checkFailed ? 'Check failed' : (!st.hasChecked ? 'Not checked yet' : (up ? 'Connection accepted · ' : 'Connection unsuccessful · ') + st.checkedWhen),", "A false TCP result does not identify a refusal error."),
        ("showOpenHint: settingsLike && !up,", "showOpenHint: settingsLike && !up,\n      checkFailed: st.checkFailed,", "Expose the required check-failure note without changing the status-row layout."),
    ]
    for before, after, reason in corrections:
        canvas = replace(canvas, before, after, reason, changes)

    owner = re.search(r'              <div class="bp-card bp-card--flush"[^\n]*>Owner</span>[^\n]*</div>\n', canvas)
    if owner is None:
        raise ValueError("The source's fictional owner row was not found")
    canvas = replace(canvas, owner.group(), "", "The settings API supplies no owner identity.", changes)
    footer = '              <x-import component-from-global-scope="BlueprintDesignSystem_ac1da4.Button" variant="secondary" onClick="{{ closeAll }}" hint-size="auto,36px">Cancel</x-import>'
    refresh = '              <sc-if value="{{ isFailed }}" hint-placeholder-val="{{ false }}"><x-import component-from-global-scope="BlueprintDesignSystem_ac1da4.Button" variant="secondary" onClick="{{ closeAll }}" hint-size="auto,36px">Refresh settings</x-import></sc-if>\n'
    canvas = replace(canvas, footer, refresh + footer, "Unconfirmed enable outcomes offer an explicit settings refresh.", changes)
    hint = '              <sc-if value="{{ showOpenHint }}"'
    failed_note = '              <sc-if value="{{ checkFailed }}" hint-placeholder-val="{{ false }}"><p style="margin:0;font-size:var(--type-caption-size);line-height:1.5;color:var(--text-tertiary);text-wrap:pretty">Deck couldn’t complete the connectivity check. Try again.</p></sc-if>\n'
    canvas = replace(canvas, hint, failed_note + hint, "A failed check is visible, without pretending the server refused a connection.", changes)
    canvas = replace(canvas, 'box-sizing:border-box;overflow:hidden;border:var(--hairline);border-radius:{{ frameR }};',
                     'box-sizing:border-box;overflow:hidden;border:0;border-radius:0;',
                     "The prototype's hardware frame consumes two mobile pixels and clips the app's bottom corners; a browser viewport has neither bezel nor hardware radius.", changes)
    canvas = replace(canvas, '<div class="pv-frame" data-viewport="{{ viewport }}" style="position:relative;',
                     '<div class="pv-frame" data-viewport="{{ viewport }}" style="position:absolute;left:0;top:0;',
                     "Place the unchanged app canvas at the browser viewport origin; export harness layout otherwise introduces a fractional-pixel raster offset.", changes)
    canvas = replace(canvas, '<span class="bp-menuitem__meta">',
                     '<span class="bp-menuitem__meta" style="font-variant-caps:normal;letter-spacing:normal">',
                     "Observed status uses sentence case instead of the design system's menu-meta small caps.", changes, count=4)
    return canvas, changes


def compose_menus(canvas, workspace, changes):
    disabled_rules = "\n".join(re.findall(r'    \.bp-menuitem\[aria-disabled="true"\][^\n]+', workspace))
    if len(disabled_rules.splitlines()) != 2:
        raise ValueError("The source menu's disabled-state styles were not found")
    canvas = replace(canvas, '    .bp-menu{--row-py:10px;--row-px:10px;--row-icon-gap:10px}',
                     '    .bp-menu{--row-py:10px;--row-px:10px;--row-icon-gap:10px}\n' + disabled_rules,
                     "Carry the source reorder controls' disabled-state CSS along with their markup.", changes)
    start = workspace.index('              <div style="display:grid;grid-template-columns:repeat(8,15px);')
    rename_at = workspace.index('<button type="button" onClick="{{ p.openRename }}"', start)
    end = workspace.rfind("\n", 0, rename_at) + 1
    prefix = workspace[start:end]
    bindings = {"p.swatches": "menuSwatches", "p.hasColor": "menuHasColor", "p.color": "menuColor",
                "p.atTop": "menuAtTop", "p.atBottom": "menuAtBottom", "p.moveUpHint": "menuMoveUpHint",
                "p.moveDownHint": "menuMoveDownHint", "p.applyColor": "stop", "p.moveUp": "stop", "p.moveDown": "stop"}
    for before, after in bindings.items():
        prefix = prefix.replace("{{ " + before + " }}", "{{ " + after + " }}")
    prefix = replace(prefix, '<sc-if value="{{ menuHasColor }}" hint-placeholder-val="{{ false }}">', '',
                     "The existing no-color command clears session overrides; retain that action for the uncolored fixture.", changes)
    prefix = replace(prefix, '              </sc-if>\n', '', "Keep the existing clear-color action visible in the composed menu.", changes)
    prefix = replace(prefix, 'Apply color to sessions', 'Clear color from sessions',
                     "An uncolored project's propagation action clears session colors.", changes)
    prefix = replace(prefix, 'background:{{ menuColor }}', 'background:transparent;border:1px solid var(--border-strong)',
                     "Use the source color indicator anatomy with an empty outlined indicator for clearing colors.", changes)
    move_up = re.search(r' +<button[^\n]*>Move up</button>', prefix).group()
    move_top = move_up.replace("menuMoveUpHint", "menuMoveTopHint").replace("Move up</button>", "Move to top</button>")
    boundary = prefix.rfind("\n", 0, prefix.rfind('<hr class="bp-menu__sep">')) + 1
    prefix = prefix[:boundary] + move_top + "\n" + prefix[boundary:]
    tags = re.search(r'const TAGS = \[[\s\S]*?\n\];', workspace).group()
    canvas = replace(canvas, "const URL_ =", tags + "\nconst URL_ =", "Compose the existing menu's palette from the uploaded desktop workspace comp.", changes)
    menu_values = """      menuHasColor: false, menuColor: 'var(--text-secondary)', menuAtTop: false, menuAtBottom: false,
      menuMoveUpHint: 'Swap places with the project above', menuMoveDownHint: 'Swap places with the project below', menuMoveTopHint: 'Move to the top of the sidebar',
      menuSwatches: TAGS.map(t => ({ name:t.name, bg:t.hex || 'transparent', ring:!t.hex ? 'var(--text-primary)' : 'transparent', mark:!t.hex ? 'inset 0 0 0 1px var(--surface-card)' : 'none', pick:e => e.stopPropagation() })),
"""
    canvas = replace(canvas, "      menuTop: '116px',", menu_values + "      menuTop: '116px',", "Use an uncolored project fixture with neighbors above and below for the existing menu's palette and reorder boundaries.", changes)
    desktop_rename = '          <button type="button" role="menuitem" class="bp-menuitem"><span style="flex:1">Rename</span><span style="flex:none;color:var(--text-tertiary)">⌘R</span></button>'
    old_rename_start = end
    old_rename_end = workspace.index("</button>", old_rename_start) + len("</button>")
    old_rename = workspace[old_rename_start:old_rename_end].replace("{{ p.openRename }}", "{{ stop }}")
    shortcut = re.search(r'<span style="flex:none;display:inline-flex;align-items:center;gap:0.34em;[\s\S]*?</span></span>', old_rename).group()
    old_rename = replace(old_rename, shortcut, '<span style="flex:none;color:var(--text-tertiary);white-space:nowrap">Ctrl+R</span>',
                         "The Linux fixture uses Ctrl+R without the Mac modifier's optical glyph offsets; preserve the source shortcut's interface face and color.", changes)
    canvas = replace(canvas, desktop_rename, prefix + old_rename, "Compose the full existing desktop menu before its new preview group; retain swatches, reorder actions and the original shortcut anatomy. Move to top reuses the source Move up row.", changes)
    mobile_rename = '            <button type="button" role="menuitem" class="bp-menuitem">Rename</button>'
    canvas = replace(canvas, mobile_rename, prefix + mobile_rename, "Keep the same existing controls inside the uploaded mobile sheet anatomy.", changes)
    return canvas


def state_specs():
    specs = {}
    for name in ["enable", "enabling", "failed", "noports", "setup", "settings", "checking", "up", "down", "checkfailed", "disableconfirm", "disablefailed", "disabled", "unconfigured", "settings-details", "up-details", "down-details", "checkfailed-details", "menu-disabled", "menu-enabled", "menu-unconfigured"]:
        base = name.removesuffix("-details")
        step = "menu" if base.startswith("menu-") else "settings" if base in ["checking", "up", "down", "checkfailed"] else base
        specs[name] = {
            "step": step, "configured": base not in ["unconfigured", "menu-unconfigured"],
            "enabled": base in ["setup", "settings", "checking", "up", "down", "checkfailed", "disableconfirm", "disablefailed", "menu-enabled"],
            "serverUp": base == "up", "checking": base == "checking", "checkFailed": base == "checkfailed",
            "hasChecked": base in ["up", "down"], "copiedUrl": False, "tabOpened": False,
            "checkedWhen": CHECKED_WHEN, "detailsOpen": name.endswith("-details"),
        }
    return specs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source_zip", type=Path)
    parser.add_argument("output_dir", type=Path)
    parser.add_argument("--offline-deps", type=Path, required=True, help="Directory with the export's integrity-verified react.js, react_dom.js and babel.js")
    parser.add_argument("--base-url", default="http://127.0.0.1:8899/preview.html")
    args = parser.parse_args()
    source_bytes = args.source_zip.read_bytes()
    if digest(source_bytes) != SOURCE_SHA256:
        raise ValueError("Source archive differs from the accepted project preview handoff")
    if args.output_dir.exists() and any(args.output_dir.iterdir()):
        raise ValueError("Output directory must be empty; preserve previous artifacts and use a new directory")
    manifest = {"sourceArchive": str(args.source_zip.resolve()), "sourceSha256": SOURCE_SHA256,
                "sourceComp": SOURCE_COMP, "sourceCompSha256": SOURCE_COMP_SHA256,
                "purpose": "Complete dialog/menu comparisons from the uploaded design, with explicit semantic corrections. No implementation CSS, markup, or screenshots are reference inputs.",
                "toolVersion": "0.14.1", "transforms": [], "dependencies": [], "preservedMembers": [], "states": []}
    members = {}
    with zipfile.ZipFile(args.source_zip) as source:
        source_comp = source.read(SOURCE_COMP)
        if digest(source_comp) != SOURCE_COMP_SHA256:
            raise ValueError("Unexpected source comp")
        canvas, manifest["transforms"] = corrected_canvas(source_comp.decode())
        menu_comp = source.read(MENU_COMP)
        if digest(menu_comp) != MENU_COMP_SHA256:
            raise ValueError("Unexpected desktop workspace menu source")
        manifest["menuSource"] = {"file": MENU_COMP, "sha256": MENU_COMP_SHA256,
                                  "composition": "Existing source swatches, apply-color and reorder prefix; Move to top uses the source Move up row anatomy; new preview rows and mobile sheet stay from Project Preview Flow."}
        canvas = compose_menus(canvas, menu_comp.decode(), manifest["transforms"])
        support = source.read("support.js").decode()
        manifest["sourceSupportSha256"] = digest(support.encode())
        for name, filename in [("REACT", "react.js"), ("REACT_DOM", "react_dom.js"), ("BABEL", "babel.js")]:
            url = re.search(r'\bvar ' + name + r'_URL = "([^"]+)";', support).group(1)
            sri = re.search(r'\bvar ' + name + r'_SRI = "(sha384-[^"]+)";', support).group(1)
            body = (args.offline_deps / filename).read_bytes()
            if "sha384-" + base64.b64encode(hashlib.sha384(body).digest()).decode() != sri:
                raise ValueError(f"{filename} does not match the original runtime's SRI declaration")
            target = "deps/" + filename
            support = support.replace('var ' + name + '_URL = "' + url + '";', 'var ' + name + '_URL = "' + target + '";')
            members[target] = body
            manifest["dependencies"].append({"from": url, "to": target, "integrity": sri, "sha256": digest(body)})
        members["support.js"] = support.encode()
        manifest["derivedSupportSha256"] = digest(members["support.js"])
        # Only the selected comp's shared design system, brand, icons and fonts
        # are carried forward. Other exported canvases never enter the import.
        for name in source.namelist():
            if name.startswith(DS_PREFIX) or name.startswith("assets/"):
                body = source.read(name)
                members[name] = body
                manifest["preservedMembers"].append({"path": name, "sha256": digest(body)})

    config = {"version": 1, "states": {}}
    for device, viewport in [("desktop", {"width": 1180, "height": 720, "fullPage": False}), ("mobile", {"width": 393, "height": 852, "fullPage": False})]:
        for name, state in state_specs().items():
            initial = {**state, "device": device}
            state_name = f"project-preview-{device}-{name}"
            comp_name = state_name
            root = "menu" if name.startswith("menu-") else "dialog"
            local_changes = []
            rendered = re.sub(r"  state = \{ step:'idle',[\s\S]*?detailsOpen:false \};", "  state = " + json.dumps(initial, ensure_ascii=False) + ";", canvas, count=1)
            if rendered == canvas:
                raise ValueError("Initial DCLogic state was not found")
            local_changes.append({"reason": "Initialize the declared demonstration state without timers or Deck runtime code.", "initialState": initial})
            if root == "dialog":
                before = '<div class="bp-dialog" role="dialog" aria-modal="true"'
            elif device == "desktop":
                before = '<div class="bp-menu" role="menu"'
            else:
                before = '<div class="bp-dialog" role="menu"'
            after = before + ' data-screen-label="' + root + '"'
            rendered = replace(rendered, before, after, "Annotate the complete design surface for the pinned importer's crop.", local_changes)
            filename = comp_name + ".dc.html"
            members[filename] = rendered.encode()
            query = "project-preview=" + name.removesuffix("-details")
            if name.endswith("-details"):
                query += "&project-preview-details=1"
            selector = "[data-project-preview-dialog]" if root == "dialog" else "[data-project-menu-popover]"
            config["states"][state_name] = {
                "route": {"url": args.base_url + ("&" if "?" in args.base_url else "?") + query,
                          "setupScript": "web/tools/project-preview-design-capture.mjs"},
                "comp": comp_name + "#" + root, "viewport": viewport, "clip": "[data-project-preview-capture-frame]",
                "readiness": {"policy": "networkidle", "selector": selector, "compSelector": '[data-screen-label="' + root + '"]', "timeout": 10000, "settle": 250},
                "threshold": 0.5,
            }
            manifest["states"].append({"state": state_name, "comp": comp_name + "#" + root,
                                       "file": filename, "sha256": digest(members[filename]), "viewport": viewport,
                                       "surface": root, "transforms": local_changes})

    manifest["captureOnlyStates"] = []
    for device, viewport in [("desktop", {"width": 1180, "height": 720, "fullPage": False}), ("mobile", {"width": 393, "height": 852, "fullPage": False})]:
        for name in ["static", "static-only", "loading"]:
            state_name = f"project-preview-{device}-{name}"
            config["states"][state_name] = {
                "route": {"url": args.base_url + ("&" if "?" in args.base_url else "?") + "project-preview=" + name,
                          "setupScript": "web/tools/project-preview-design-capture.mjs"},
                "comp": None, "viewport": viewport, "clip": "[data-project-preview-capture-frame]",
                "readiness": {"policy": "networkidle", "selector": "[data-project-preview-dialog]", "timeout": 10000, "settle": 250},
                "threshold": 0.5,
            }
            manifest["captureOnlyStates"].append({"state": state_name, "reason": "No supplied reference for this backend-required state; capture is review evidence, not a convergence verdict."})
    args.output_dir.mkdir(parents=True, exist_ok=True)
    archive_path = args.output_dir / "project-preview-references.zip"
    with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, body in sorted(members.items()):
            info = zipfile.ZipInfo(name, date_time=(2026, 9, 20, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            archive.writestr(info, body)
    manifest["derivedArchiveSha256"] = digest(archive_path.read_bytes())
    manifest["configSha256"] = digest((json.dumps(config, indent=2) + "\n").encode())
    (args.output_dir / "provenance.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
    (args.output_dir / "config-fragment.json").write_text(json.dumps(config, indent=2) + "\n")
    print(archive_path)
    print(f"{len(manifest['states'])} complete reference states and {len(manifest['captureOnlyStates'])} capture-only states; source and every transformation recorded in provenance.json")


if __name__ == "__main__":
    main()
