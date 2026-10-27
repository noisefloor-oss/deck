// ConsoleSettings mirrors internal/settings.Settings: user choices
// the UI writes at runtime, as opposed to deployment configuration.
// Each field names the confirmation or permission check it skips. PUT
// replaces the whole document; omitted fields write false.
export interface ConsoleSettings {
  skip_permissions: boolean;
  skip_close_confirm: boolean;
  skip_remove_project_confirm: boolean;
}

// SidebarLayout mirrors internal/settings.Layout: the user's sidebar
// arrangement, stored as its own document so a Settings PUT cannot overwrite
// it. Stale ids from removed projects or sessions round-trip harmlessly.
export interface SidebarLayout {
  expanded: string[];
  projects: string[];
  sessions: Record<string, string[]>;
  // colors maps a project id to its user color tag. Absent means no
  // tag; an empty map and a missing field read the same.
  colors?: Record<string, string>;
}

// TutorialPreference mirrors internal/settings.TutorialPreference: the
// user-scoped record of tutorial presentation state — whether they
// closed it and where to resume. version is the contract version (this
// build speaks 1); a record read back with any other version is not
// rewritten, so the field stays a number rather than a literal. done lists
// finished lesson ids, the same ids the catalog ships, and "welcomed" once
// the user has started the tutorial from its welcome page.
export interface TutorialPreference {
  version: number;
  dismissed: boolean;
  lesson: string;
  step: number;
  done: string[];
}

// TutorialRecord is the GET/PUT /api/settings/tutorial answer: the stored
// document plus the server-derived scope it was read or written under.
// "installation" means an unauthenticated deployment's single shared
// record; the server ignores any client-supplied scope on write.
// "operator" remains accepted for compatibility with the previous release.
export interface TutorialRecord extends TutorialPreference {
  scope: "user" | "operator" | "installation";
}
