// Shared types keep parsers independent from the React sidebar component.

export type FileTouchKind = 'read' | 'write' | 'edit';

export type ChecklistStatus = 'done' | 'active' | 'pending';

export interface SidebarChecklistItem {
  text: string;
  status: ChecklistStatus;
}

export interface SidebarFileEntry {
  filename: string;
  kind: FileTouchKind;
}
