declare module 'somewhere/db' {
  export const schema: (tables: Record<string, unknown>) => unknown;
  export const table: (columns: Record<string, unknown>, options: Record<string, unknown>) => unknown;
  export const id: (options?: Record<string, unknown>) => unknown;
  export const text: (options?: Record<string, unknown>) => unknown;
  export const timestamp: (options?: Record<string, unknown>) => unknown;
  export const json: (options?: Record<string, unknown>) => unknown;
  export const owner: (options?: Record<string, unknown>) => unknown;
}
