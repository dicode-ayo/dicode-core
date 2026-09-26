/**
 * Run mirrors registry.Run from the Go side: the struct carries no json tags,
 * so field names reach the wire as Go's default PascalCase rather than the
 * snake_case the rest of the API uses. Every field is optional in both
 * spellings so a spec can decode whichever subset of the run a given endpoint
 * actually returns.
 */
export interface Run {
  ID?: string;
  id?: string;
  TaskID?: string;
  task_id?: string;
  Status?: string;
  status?: string;
  ParentRunID?: string;
  parent_run_id?: string;
  TriggerSource?: string;
  trigger_source?: string;
  ReturnValue?: string;
  return_value?: string;
}

export function runID(r: Run): string {
  return (r.ID ?? r.id) as string;
}

export function runStatus(r: Run): string {
  return (r.Status ?? r.status) as string;
}

export function runReturnValue(r: Run): string {
  return (r.ReturnValue ?? r.return_value ?? '') as string;
}
