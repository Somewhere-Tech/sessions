// Shared types between parsers and the Sessions derived sidebar.
//
// Lifted out of the original sessions-tmux StatusSidebar component so parsers
// don't need to import a React component to get the types of their findings.

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
