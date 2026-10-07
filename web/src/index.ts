// Public entry point for @projectbooth/api-ui (ADR 0030). The shell builds one stylesheet by
// scanning this package's compiled JS for class names (ADR 0097), so class names here must stay
// complete static strings; dist/style.css carries only what scanning can't regenerate.
import "./library.css";

export { ApiApp } from "./ApiApp";
export type { ApiAppProps } from "./ApiApp";
export type { WorkspaceRole } from "./types";
