import type { ApiClient } from "../../api/contracts.ts";
import type { TutorialPreference, TutorialRecord } from "../../api/settings.ts";
import { sequenceDone, type EventFacts } from "./events.ts";
import type { LessonPanel, StepSettings } from "./lessons.ts";

// Lessons are data files bundled at build time (lesson-files.ts);
// this module holds the model that walks a catalog and persists the
// position, and knows nothing about where the catalog came from.

export interface TutorialStep extends StepSettings {
  title: string;
  // Markdown source with {placeholders}; the panel substitutes and renders it.
  body: string;
  // Marks the closing page appended after a lesson's last step.
  done?: { next: string };
}

export interface TutorialLesson {
  // id is the catalog key the preference record persists in its lesson and
  // done fields; the backend bounds it to short lowercase ids, never prose.
  id: string;
  title: string;
  // Optional lessons (the Explore Deck chapters) ship after the guided
  // course but are never entered by Next: the last step of either group
  // finishes the walkthrough, and only an explicit lesson choice opens an
  // optional one.
  optional?: boolean;
  duration?: string;
  requires?: readonly string[];
  principles?: readonly number[];
  steps: readonly TutorialStep[];
}

export type TutorialPhase = "loading" | "ready" | "suspended" | "disposed";

export interface TutorialState {
  phase: TutorialPhase;
  // The panel is on screen now: not pending the initial read, not
  // persistently dismissed, and not collapsed for this session.
  visible: boolean;
  dismissed: boolean;
  // Session-only collapse: hides the panel without writing a dismissal, so
  // a reload brings it back.
  collapsed: boolean;
  // The step page is unfolded over an open Files, Tasks or Chats panel.
  // Always false while no panel is open.
  stepShown: boolean;
  // Reopened partway through a lesson: the panel offers Resume before the
  // step itself. Presentation only; nothing about it is saved.
  resumePage: boolean;
  // The welcome page stands in front of the first lesson until the user
  // starts the tutorial from it.
  welcome: boolean;
  lesson: string;
  step: number;
  // How many times an observed event moved the lesson on by itself.
  advances: number;
  done: readonly string[];
  scope: "user" | "operator" | "installation" | null;
  loadError: string | null;
  saveError: string | null;
  // The stored record speaks a version this build does not. Nothing is
  // written and the record is left exactly as read.
  unsupportedVersion: boolean;
}

export interface TutorialHost {
  onChange?: (state: TutorialState) => void;
  // A failed save is reported here even once the panel is closed, so
  // you still learn your position was not kept. Fired once per
  // failed write; there is no automatic retry.
  onSaveError?: (message: string) => void;
  // Steps this predicate hides are skipped by Next and Back and left out of
  // the step count.
  hidden?: (step: TutorialStep) => boolean;
  // Called once when the user arrives on a step that opens a panel.
  openPanel?: (panel: LessonPanel) => void;
  // What Deck has observed, for steps that complete on an event.
  facts?: EventFacts;
}

const KNOWN_TUTORIAL_VERSION = 1;

// Recorded in done once the user starts the tutorial from the welcome page.
export const WELCOMED = "welcomed";

// DOM-free model for the walkthrough panel: it owns presentation state and
// its persistence, nothing else. The server owns the record; every write
// replaces the whole document, and saves are serialized so only the latest
// gesture's state goes out. Races are fenced per concern — a slow initial
// read can neither reopen a panel you closed nor re-dismiss one
// you reopened, and it cannot move a position you are navigating.
export class TutorialModel {
  private readonly api: ApiClient;
  readonly catalog: readonly TutorialLesson[];
  private readonly host: TutorialHost;

  private phase: TutorialPhase = "loading";
  private started = false;
  // Bumped by every lifecycle edge (load, suspend, resume, dispose); async
  // continuations act only while their generation is still current.
  private generation = 0;
  private dismissed = false;
  private collapsed = false;
  private stepShown = false;
  private resuming = false;
  private welcomed = false;
  private docked: string | null = null;
  // The step an arrival was last handled for. A step that opens a panel
  // acts once per arrival, so a panel the user closed stays closed until
  // they move to another step.
  private arrivedAt = "";
  private lessonIndex = 0;
  private step = 0;
  // The step the page loaded onto from the saved record, until the user
  // moves. A step waiting on a reload completes only there.
  private restoredAt = "";
  // A step the user went Back to does not complete again from facts that
  // were already true; it waits for one of them to happen again.
  private held: { key: string; values: number[] } | null = null;
  private adoptedOnce = false;
  private advances = 0;
  private done: string[] = [];
  private scope: TutorialRecord["scope"] | null = null;
  private loadError: string | null = null;
  private saveError: string | null = null;
  private unsupportedVersion = false;

  // Gesture fences for a slow initial read: once you have spoken,
  // the stored answer does not speak over you. close/reopen own dismissal;
  // next/back own position. Suspension clears both — the gestures whose
  // writes it drops no longer outrank the record resume() re-reads.
  private visibilitySpoken = false;
  private positionSpoken = false;

  private loadAbort: AbortController | null = null;
  // One save in flight at most; later gestures only mark state pending, and
  // the document is built from state when the request actually goes out, so
  // intermediate gestures collapse into the latest one.
  private saveInFlight = false;
  private savePending = false;
  private saveAbort: AbortController | null = null;

  constructor(
    api: ApiClient,
    catalog: readonly TutorialLesson[],
    host: TutorialHost = {},
  ) {
    this.api = api;
    this.catalog = catalog;
    this.host = host;
  }

  get state(): TutorialState {
    return {
      phase: this.phase,
      visible: this.phase === "ready" && (!this.loadError || this.visibilitySpoken) && !this.dismissed && !this.collapsed,
      dismissed: this.dismissed,
      collapsed: this.collapsed,
      stepShown: this.docked !== null && this.stepShown,
      resumePage: this.phase === "ready" && this.resuming && !this.dismissed,
      welcome: !this.welcomed && this.lessonIndex === 0,
      lesson: this.catalog[this.lessonIndex]?.id ?? "",
      step: this.step,
      advances: this.advances,
      done: [...this.done],
      scope: this.scope,
      loadError: this.loadError,
      saveError: this.saveError,
      unsupportedVersion: this.unsupportedVersion,
    };
  }

  // The initial read. Gestures may land before it returns; adopt() fences
  // them per concern.
  load(): void {
    if (this.started || this.phase === "disposed") return;
    this.started = true;
    void this.read();
  }

  close(): void {
    if (!this.mutable()) return;
    this.resuming = false;
    this.dismissed = true;
    this.visibilitySpoken = true;
    this.save();
    this.changed();
  }

  // Reopening resumes exactly where the record says you stood.
  reopen(): void {
    if (!this.mutable()) return;
    // The position is only known once the record has been read, and a
    // panel that is already on screen is not interrupted.
    this.resuming = this.phase === "ready" && (this.dismissed || this.collapsed) && this.partway();
    this.dismissed = false;
    this.collapsed = false;
    this.visibilitySpoken = true;
    this.save();
    this.changed();
  }

  // Start the tutorial from the welcome page.
  start(): void {
    if (!this.mutable() || this.welcomed) return;
    this.passWelcome();
    this.positionSpoken = true;
    this.save();
    this.changed();
  }

  private passWelcome(): void {
    this.welcomed = true;
    if (!this.done.includes(WELCOMED)) this.done.push(WELCOMED);
  }

  // The user has chosen where to go from the resume page.
  leaveResumePage(): void {
    if (!this.resuming) return;
    this.resuming = false;
    this.changed();
  }

  // Past the first step of a lesson that is not finished, and not on its
  // closing page.
  private partway(): boolean {
    const lesson = this.catalog[this.lessonIndex];
    if (lesson === undefined || this.done.includes(lesson.id) || lesson.steps[this.step]?.done !== undefined) return false;
    return this.visibleSteps(lesson).some((index) => index < this.step);
  }

  collapse(): void {
    if (!this.mutable()) return;
    this.collapsed = true;
    this.changed();
  }

  expand(): void {
    if (!this.mutable()) return;
    this.collapsed = false;
    this.changed();
  }

  // Records which panel is open beside the tutorial. The step page starts
  // folded whenever that changes, including a direct switch between panels.
  setDocked(docked: string | null): void {
    if (this.docked === docked) return;
    this.docked = docked;
    this.stepShown = false;
  }

  showStep(): void {
    if (!this.mutable() || this.docked === null) return;
    this.stepShown = true;
    this.changed();
  }

  hideStep(): void {
    if (!this.mutable() || !this.stepShown) return;
    this.stepShown = false;
    this.changed();
  }

  selectLesson(id: string): void {
    if (!this.mutable()) return;
    const index = this.catalog.findIndex((lesson) => lesson.id === id);
    if (index < 0) return;
    this.resuming = false;
    this.passWelcome();
    this.lessonIndex = index;
    this.step = this.visibleSteps(this.catalog[index]!)[0] ?? 0;
    this.positionSpoken = true;
    this.moved();
    this.settle();
    this.save();
    this.arrive();
    this.changed();
  }

  // Continue, and Next on a page with nothing to wait for: the step is done.
  next(): void {
    this.advance(true);
  }

  // Skip step: move on without the step's event having happened.
  skip(): void {
    this.advance(false);
  }

  // Tells the model that something it may be waiting for happened or
  // changed. A step completes when its events hold, and the lesson moves on
  // by one step at a time, each step checked against what is already true.
  factsChanged(): void {
    if (this.phase !== "ready" || this.dismissed || this.unsupportedVersion) return;
    if (!this.settle()) return;
    this.save();
    this.arrive();
    this.changed();
  }

  // Whether the step carries the page's reload from the saved record.
  private restored(): boolean {
    return this.restoredAt === this.currentKey();
  }

  private currentKey(): string {
    return `${this.catalog[this.lessonIndex]?.id ?? ""}:${this.step}`;
  }

  private moved(): void {
    this.held = null;
    this.restoredAt = "";
  }

  private eventValues(step: TutorialStep | undefined): number[] {
    const facts = this.host.facts;
    const context = { lesson: this.catalog[this.lessonIndex]?.id ?? "", step: this.step, restored: this.restored() };
    return facts === undefined ? [] : (step?.advanceOn ?? []).map((call) => facts.value(call, context));
  }

  // Completes steps whose events hold. Returns whether the position moved.
  private settle(): boolean {
    const facts = this.host.facts;
    if (facts === undefined) return false;
    let moved = false;
    for (let guard = 0; guard < 64; guard++) {
      const lesson = this.catalog[this.lessonIndex];
      const step = lesson?.steps[this.step];
      const events = step?.advanceOn;
      if (lesson === undefined || step === undefined || events === undefined || events.length === 0) break;
      if (events.some((call) => call.name === "continue")) break;
      const key = this.currentKey();
      if (this.held !== null && this.held.key === key) {
        const values = this.eventValues(step);
        if (!values.some((value, index) => value > (this.held!.values[index] ?? 0))) {
          this.held.values = values.map((value, index) => Math.min(value, this.held!.values[index] ?? 0));
          break;
        }
        this.held = null;
      }
      if (!sequenceDone(events, facts, { lesson: lesson.id, step: this.step, restored: this.restored() })) break;
      const ahead = this.stepAfter(lesson, this.step);
      // The last step is left by Finish, never by an event.
      if (ahead === null) break;
      this.step = ahead;
      this.moved();
      this.positionSpoken = true;
      this.noteDonePage(true);
      moved = true;
    }
    if (moved) this.advances += 1;
    return moved;
  }

  // Reaching a lesson's closing page by finishing its steps records the
  // lesson. Skipping to it does not.
  private noteDonePage(completed: boolean): void {
    const lesson = this.catalog[this.lessonIndex];
    if (!completed || lesson === undefined || lesson.steps[this.step]?.done === undefined) return;
    if (!this.done.includes(lesson.id)) this.done.push(lesson.id);
  }

  private advance(completed: boolean): void {
    if (!this.mutable()) return;
    const lesson = this.catalog[this.lessonIndex];
    if (lesson === undefined) return;
    this.positionSpoken = true;
    this.moved();
    const ahead = this.stepAfter(lesson, this.step);
    if (ahead !== null) {
      this.step = ahead;
      this.noteDonePage(completed);
    } else {
      // Next never crosses between the guided course and the optional
      // Explore Deck chapters: the last step of either group finishes.
      const follower = this.catalog[this.lessonIndex + 1];
      if (follower !== undefined && (follower.optional ?? false) === (lesson.optional ?? false)) {
        if (!this.done.includes(lesson.id)) this.done.push(lesson.id);
        this.lessonIndex += 1;
        this.step = this.visibleSteps(follower)[0] ?? 0;
      } else {
        // Leaving the group's last step finishes the walkthrough.
        if (!this.done.includes(lesson.id)) this.done.push(lesson.id);
        this.dismissed = true;
        this.visibilitySpoken = true;
      }
    }
    this.settle();
    this.save();
    this.arrive();
    this.changed();
  }

  back(): void {
    if (!this.mutable() || this.starting) return;
    const behind = this.stepBefore(this.catalog[this.lessonIndex]!, this.step);
    this.positionSpoken = true;
    this.moved();
    if (behind !== null) {
      this.step = behind;
    } else {
      const previous = this.catalog[this.lessonIndex - 1]!;
      this.lessonIndex -= 1;
      this.step = this.stepBefore(previous, previous.steps.length) ?? 0;
    }
    this.held = { key: this.currentKey(), values: this.eventValues(this.catalog[this.lessonIndex]?.steps[this.step]) };
    this.save();
    this.arrive();
    this.changed();
  }

  // Back stops at the first step of the guided course or optional chapters.
  get starting(): boolean {
    const lesson = this.catalog[this.lessonIndex];
    if (lesson === undefined) return true;
    if (this.stepBefore(lesson, this.step) !== null) return false;
    const previous = this.catalog[this.lessonIndex - 1];
    return previous === undefined || (previous.optional ?? false) !== (lesson.optional ?? false);
  }

  // True when Next would finish the walkthrough rather than advance — the
  // last step of the guided course or of the optional chapters. The panel
  // labels that button Finish.
  get finishing(): boolean {
    const lesson = this.catalog[this.lessonIndex];
    if (lesson === undefined || this.stepAfter(lesson, this.step) !== null) return false;
    const follower = this.catalog[this.lessonIndex + 1];
    return follower === undefined || (follower.optional ?? false) !== (lesson.optional ?? false);
  }

  // Indices of the steps the panel shows, in order.
  visibleSteps(lesson: TutorialLesson): number[] {
    const hidden = this.host.hidden;
    return lesson.steps.flatMap((step, index) => (hidden?.(step) === true ? [] : [index]));
  }

  private stepAfter(lesson: TutorialLesson, from: number): number | null {
    return this.visibleSteps(lesson).find((index) => index > from) ?? null;
  }

  private stepBefore(lesson: TutorialLesson, from: number): number | null {
    return this.visibleSteps(lesson).filter((index) => index < from).pop() ?? null;
  }

  // Controller loss: interest in in-flight work ends here. An aborted write
  // may still have been accepted server-side — nothing assumes it was not.
  // Queued work is dropped, and resume() re-reads the record rather than
  // replaying it. The session-only collapse survives; a controller blip
  // within one page must not pop a collapsed panel back open.
  suspend(): void {
    if (this.phase === "disposed" || this.phase === "suspended") return;
    this.generation++;
    this.phase = "suspended";
    this.saveInFlight = false;
    this.savePending = false;
    this.saveAbort?.abort();
    this.saveAbort = null;
    this.loadAbort?.abort();
    this.loadAbort = null;
    this.saveError = null;
    this.visibilitySpoken = false;
    this.positionSpoken = false;
    this.changed();
  }

  // Controller regained: adopt the server's record wholesale. Another
  // client may have written since, and an aborted write may have landed.
  resume(): void {
    if (this.phase !== "suspended") return;
    void this.read();
  }

  dispose(): void {
    if (this.phase === "disposed") return;
    this.generation++;
    this.phase = "disposed";
    this.saveInFlight = false;
    this.savePending = false;
    this.saveAbort?.abort();
    this.saveAbort = null;
    this.loadAbort?.abort();
    this.loadAbort = null;
    this.changed();
  }

  // Gestures are accepted while the initial read is still out — that race
  // is exactly what the fences exist for — but not once suspended or
  // disposed.
  private mutable(): boolean {
    return this.phase === "loading" || this.phase === "ready";
  }

  // Arriving on a step folds its page and runs its `opens`. Only
  // navigation and the first adoption of a position arrive; a repaint or a
  // re-read that lands on the same step does not.
  private arrive(): void {
    if (this.phase !== "ready") return;
    this.stepShown = false;
    const key = `${this.catalog[this.lessonIndex]?.id ?? ""}:${this.step}`;
    if (key === this.arrivedAt) return;
    this.arrivedAt = key;
    if (this.dismissed) return;
    const opens = this.catalog[this.lessonIndex]?.steps[this.step]?.opens;
    if (opens !== undefined) this.host.openPanel?.(opens);
  }

  private changed(): void {
    this.host.onChange?.(this.state);
  }

  private async read(): Promise<void> {
    this.started = true;
    const generation = ++this.generation;
    this.phase = "loading";
    this.loadError = null;
    const abort = new AbortController();
    this.loadAbort = abort;
    this.changed();
    let settled = false;
    try {
      const record = await this.api.getTutorial(abort.signal);
      if (generation === this.generation) {
        settled = true;
        this.adopt(record);
      }
    } catch (err) {
      if (generation === this.generation) {
        settled = true;
        // A failed read cannot establish first use or a safe document to
        // replace. Manual opening remains available without persistence.
        this.loadError = err instanceof Error ? err.message : String(err);
      }
    } finally {
      if (settled) {
        this.phase = "ready";
        this.loadAbort = null;
        if (this.savePending && !this.loadError && !this.unsupportedVersion) this.beginSave();
        if (!this.loadError && !this.unsupportedVersion) {
          if (this.settle()) this.save();
          this.arrive();
        }
        this.changed();
      }
    }
  }

  private adopt(record: TutorialRecord | null): void {
    const first = !this.adoptedOnce;
    this.adoptedOnce = true;
    this.unsupportedVersion = false;
    if (record === null) {
      this.scope = null;
      if (!this.visibilitySpoken) this.dismissed = false;
      if (!this.positionSpoken) {
        this.lessonIndex = 0;
        this.step = 0;
        this.done = [];
        this.welcomed = false;
      }
      return;
    }
    this.scope = record.scope;
    if (record.version !== KNOWN_TUTORIAL_VERSION) {
      // A record from a build this one cannot speak. Read-only: the panel
      // stays closed (a user with a stored record has met a tutorial)
      // and no write goes out that could clobber it.
      this.unsupportedVersion = true;
      if (!this.visibilitySpoken) this.dismissed = true;
      return;
    }
    if (!this.visibilitySpoken) {
      this.dismissed = record.dismissed === true;
    }
    if (!this.positionSpoken) {
      // Missing lessons (a catalog this build no longer ships) restart at
      // the beginning; a stored step past the lesson's end clamps to its
      // last step. Dismissal is unaffected either way.
      const found = this.catalog.findIndex((lesson) => lesson.id === record.lesson);
      this.lessonIndex = found >= 0 ? found : 0;
      const lesson = this.catalog[this.lessonIndex];
      const last = lesson === undefined ? 0 : Math.max(lesson.steps.length - 1, 0);
      this.step = Number.isInteger(record.step) ? Math.min(Math.max(record.step, 0), last) : 0;
      this.done = Array.isArray(record.done) ? [...record.done] : [];
      // A record with any progress predates the welcome page or passed it.
      this.welcomed = this.done.length > 0 || this.lessonIndex !== 0 || this.step !== 0;
      if (first) this.restoredAt = this.currentKey();
    }
  }

  private save(): void {
    if (this.phase === "suspended" || this.phase === "disposed" || this.unsupportedVersion || this.loadError) return;
    // A whole-document write must wait until the stored progress and
    // version are known; closing during load must not reset the lesson.
    if (this.phase === "loading") {
      this.savePending = true;
      return;
    }
    if (this.saveInFlight) {
      this.savePending = true;
      return;
    }
    this.beginSave();
  }

  private beginSave(): void {
    const generation = this.generation;
    const abort = new AbortController();
    this.saveAbort = abort;
    this.saveInFlight = true;
    this.savePending = false;
    const document = this.preference();
    void this.api.saveTutorial(document, abort.signal).then(
      () => this.saveSettled(abort, generation, null),
      (err: unknown) => this.saveSettled(abort, generation, err),
    );
  }

  private saveSettled(abort: AbortController, generation: number, err: unknown): void {
    // Another lifecycle edge or a newer save cycle owns the slot: this
    // result is late and decides nothing, including whether the write
    // landed.
    if (this.saveAbort !== abort) return;
    this.saveAbort = null;
    this.saveInFlight = false;
    if (generation !== this.generation) return;
    if (err !== null) {
      // No automatic retry: the failure is reported and only a newer
      // gesture writes again.
      this.saveError = err instanceof Error ? err.message : String(err);
      this.host.onSaveError?.(this.saveError);
    } else {
      this.saveError = null;
    }
    if (this.savePending) {
      this.beginSave();
      return;
    }
    this.changed();
  }

  private preference(): TutorialPreference {
    return {
      version: KNOWN_TUTORIAL_VERSION,
      dismissed: this.dismissed,
      lesson: this.catalog[this.lessonIndex]?.id ?? "",
      step: this.step,
      done: [...new Set(this.done)],
    };
  }
}
