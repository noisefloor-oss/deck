// Keep an explicit rounded crop and reject frames that omit visible text or
// controls or change layout. Visual Diff 0.14.1 rounds both reference and
// implementation boxes; these containment and layout guards remain useful.
export default async function setup(page) {
  const state = new URL(page.url()).searchParams.get("project-preview") ?? "";
  const selector = state.startsWith("menu-")
    ? "[data-project-menu-popover]" : "[data-project-preview-dialog]";
  await page.waitForSelector(selector, { state: "visible" });
  await page.waitForLoadState("networkidle");
  await page.evaluate(() => document.fonts.ready);
  await page.evaluate((selector) => {
    const surface = document.querySelector(selector);
    if (!(surface instanceof HTMLElement)) throw new Error("Preview surface is missing");
    const box = surface.getBoundingClientRect();
    const crop = { x: Math.round(box.x), y: Math.round(box.y), width: Math.round(box.width), height: Math.round(box.height) };
    const contained = (rect) => rect.x >= crop.x && rect.y >= crop.y
      && rect.right <= crop.x + crop.width && rect.bottom <= crop.y + crop.height;
    for (const control of surface.querySelectorAll("button,input,select,textarea,a[href]")) {
      if (control.getClientRects().length && !contained(control.getBoundingClientRect())) {
        throw new Error("The rounded preview crop would omit a control");
      }
    }
    const walker = document.createTreeWalker(surface, NodeFilter.SHOW_TEXT);
    while (walker.nextNode()) {
      if (!walker.currentNode.textContent.trim()) continue;
      const range = document.createRange();
      range.selectNodeContents(walker.currentNode);
      for (const rect of range.getClientRects()) {
        if (rect.width && rect.height && !contained(rect)) throw new Error("The rounded preview crop would omit text");
      }
    }
    const frame = document.createElement("div");
    frame.dataset.projectPreviewCaptureFrame = "";
    frame.setAttribute("aria-hidden", "true");
    frame.inert = true;
    Object.assign(frame.style, { position: "absolute", pointerEvents: "none", opacity: "0", margin: "0", padding: "0", border: "0",
      left: `${crop.x + window.scrollX}px`, top: `${crop.y + window.scrollY}px`, width: `${crop.width}px`, height: `${crop.height}px` });
    document.body.append(frame);
    const after = surface.getBoundingClientRect();
    if (["x", "y", "width", "height"].some(key => after[key] !== box[key])) throw new Error("Capture framing changed the preview layout");
  }, selector);
}
