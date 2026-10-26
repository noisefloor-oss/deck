// Coordinates come from the rendered cell grid, not the terminal's outer
// padding. Buffer offsets keep selection aligned when viewing scrollback.
export function touchSelectionCell(
  point: { clientX: number; clientY: number },
  box: { left: number; top: number; width: number; height: number },
  grid: { columns: number; rows: number; viewportY: number },
): number | null {
  if (box.width <= 0 || box.height <= 0 || grid.columns <= 0 || grid.rows <= 0) return null;
  const column = Math.max(0, Math.min(grid.columns - 1,
    Math.floor((point.clientX - box.left) / box.width * grid.columns)));
  const row = Math.max(0, Math.min(grid.rows - 1,
    Math.floor((point.clientY - box.top) / box.height * grid.rows)));
  return (grid.viewportY + row) * grid.columns + column;
}

export function touchSelectionRange(anchor: number, current: number, columns: number): { column: number; row: number; length: number } {
  const first = Math.min(anchor, current);
  return { column: first % columns, row: Math.floor(first / columns), length: Math.abs(current - anchor) + 1 };
}
