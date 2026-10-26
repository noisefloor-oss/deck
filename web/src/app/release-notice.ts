// A public release notification, separate from the browser-build reload action.
// Only dismissal of a release is stored in this browser.
import { fetchWithAuthorization } from "../api/http.ts";
export const releaseDismissalKey = "noisedeck.release.dismissed";
const checkInterval = 6 * 60 * 60 * 1000;

export function isStableReleaseBuild(buildID: string | null): boolean {
  return buildID !== null && /^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$/.test(buildID);
}

export interface ReleaseUpdate {
  version: string;
  release_url: string;
}

export function validReleaseUpdate(value: unknown): value is ReleaseUpdate {
  if (!value || typeof value !== "object") return false;
  const notice = value as ReleaseUpdate;
  return typeof notice.version === "string"
    && /^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$/.test(notice.version)
    && notice.release_url === `https://github.com/noisefloor-oss/deck/releases/tag/${notice.version}`;
}

export function watchReleaseNotice(root: HTMLElement, authorizationLost?: () => void): () => void {
  const region = root.querySelector<HTMLElement>("[data-release-notice]");
  if (!region) return () => {};
  let disposed = false;
  let dismissed = "";
  const abort = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const check = async () => {
    try {
      const response = await fetchWithAuthorization("/api/release-update", {
        credentials: "same-origin", cache: "no-store", signal: abort.signal,
      }, () => {
        if (!disposed) authorizationLost?.();
      });
      if (disposed) return;
      if (!response.ok) return;
      const notice: unknown = await response.json();
      if (disposed) return;
      region.replaceChildren();
      region.hidden = true;
      if (!validReleaseUpdate(notice) || dismissed === notice.version) return;
      try {
        if (localStorage.getItem(releaseDismissalKey) === notice.version) return;
      } catch { /* Storage may be blocked. */ }
      const link = document.createElement("a");
      link.href = notice.release_url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = `Deck ${notice.version.slice(1)} available`;
      link.setAttribute("aria-label", `Deck ${notice.version.slice(1)} available. View release notes`);
      link.className = "underline underline-offset-2";
      const dismiss = document.createElement("button");
      dismiss.type = "button";
      dismiss.textContent = "×";
      dismiss.setAttribute("aria-label", "Dismiss update notice");
      dismiss.className = "self-stretch px-3 text-body-sm pointer-coarse:min-w-10";
      dismiss.onclick = () => {
        dismissed = notice.version;
        try {
          localStorage.setItem(releaseDismissalKey, notice.version);
        } catch { /* The in-memory dismissal still lasts for this mount. */ }
        region.hidden = true;
      };
      region.append(link, dismiss);
      region.hidden = false;
    } catch { /* An offline release check never interrupts work. */ }
    finally {
      if (!disposed) timer = setTimeout(() => { void check(); }, checkInterval);
    }
  };
  void check();
  return () => {
    disposed = true;
    abort.abort();
    if (timer !== undefined) clearTimeout(timer);
    region.replaceChildren();
    region.hidden = true;
  };
}
