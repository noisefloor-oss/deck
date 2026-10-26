// MobileTerminalCompose buffers phone text, pastes it, then presses Enter.
//
// The row is a real text field — the one exception to terminal-only
// input: text accumulates in the field — so dictation and voice-narration apps, which
// edit a field's value rather than emitting keystrokes, work — and nothing
// reaches the session until Send or Return flushes the buffer. Control
// keys stay with the key strip; the terminal itself remains the only
// surface where a keystroke leaves as it is typed.
interface ComposeTerminal {
  trackWrite(send: () => void, done: (code: string, message: string) => void): boolean;
  paste(text: string): void;
  sendInput(data: string): void;
}

export class MobileTerminalCompose {
  private readonly terminal: ComposeTerminal;
  private disposed = false;
  private pending = false;
  private generation = 0;

  constructor(terminal: ComposeTerminal) {
    this.terminal = terminal;
  }

  // Use xterm's native paste handling so applications that enable bracketed
  // paste can distinguish draft line breaks from the subsequent submit key.
  // Both calls are synchronous; no timer or provider-specific key is needed.
  // An empty buffer still answers a TUI prompt that only wants Enter.
  flush(value: string, done: (code: string, message: string) => void): boolean {
    if (this.disposed || this.pending) return false;
    const writes = value === ""
      ? [() => this.terminal.sendInput("\r")]
      : [() => this.terminal.paste(value), () => this.terminal.sendInput("\r")];
    const generation = ++this.generation;
    let remaining = writes.length;
    let outcome = { code: "sent", message: "" };
    let completed = false;
    this.pending = true;
    const complete = (code: string, message: string): void => {
      if (this.disposed || generation !== this.generation || completed) return;
      if (outcome.code === "sent" && code !== "sent") outcome = { code, message };
      remaining--;
      if (remaining !== 0) return;
      completed = true;
      this.pending = false;
      done(outcome.code, outcome.message);
    };
    for (let index = 0; index < writes.length; index++) {
      if (!this.terminal.trackWrite(writes[index]!, complete)) {
        if (index === 0) {
          this.pending = false;
          return false;
        }
        complete("failed", "The submit key was not sent.");
      }
    }
    return true;
  }

  dispose(): void {
    this.disposed = true;
    this.pending = false;
    this.generation++;
  }
}

// isComposeSubmitIntent names the input intents that flush the buffer
// instead of editing it: the soft keyboard's Return. Every other intent —
// typing, dictation's composition and text insertions, deletions inside
// the draft — belongs to the field and is left alone.
export function isComposeSubmitIntent(inputType: string): boolean {
  return inputType === "insertLineBreak" || inputType === "insertParagraph";
}
