import type { SessionSummary } from "../api/sessions.ts";

export interface ConversationAttention {
  session_id: string;
  generation: number;
  context_key: string;
  state: "unknown" | "working" | "needs_input" | "ended";
  sequence: number;
  ended_sequence: number;
  seen_sequence: number;
}

interface AcknowledgementOptions {
  current: () => SessionSummary | undefined;
  visible: () => boolean;
  schedule: (callback: () => void) => void;
  acknowledge: (session: string, fact: ConversationAttention) => Promise<void>;
  changed: () => void;
}

// Viewing acknowledges only the exact completed boundary the server served.
// Pending native input is independent and is never cleared by this receipt.
export class ConversationAcknowledgements {
  private pending = new Set<string>();
  private disposed = false;
  private readonly options: AcknowledgementOptions;

  constructor(options: AcknowledgementOptions) {
    this.options = options;
  }

  observe(): void {
    if (this.disposed) return;
    const session = this.options.current();
    const fact = session?.conversation_attention;
    if (!session || !fact || fact.session_id !== session.id || fact.generation !== session.generation || fact.ended_sequence <= fact.seen_sequence) return;
    if (!this.options.visible()) return;
    const key = `${session.id}:${fact.generation}:${fact.context_key}:${fact.ended_sequence}`;
    if (this.pending.has(key)) return;
    this.pending.add(key);
    this.options.schedule(() => {
      const current = this.options.current();
      if (this.disposed || !this.options.visible() || current?.id !== session.id || current.generation !== fact.generation || current.conversation_attention?.context_key !== fact.context_key) {
        this.pending.delete(key); return;
      }
      void this.options.acknowledge(session.id, fact).then(() => {
        if (!this.disposed) this.options.changed();
      }).catch(() => {}).finally(() => this.pending.delete(key));
    });
  }
  dispose(): void { this.disposed = true; this.pending.clear(); }
}
