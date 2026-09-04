// `overflow-x-auto`, not `overflow-hidden`: a table wider than its column
// SCROLLS rather than being silently cut off (the objects browser's two 64-hex
// columns lost the Created column entirely at 1080p, with no scrollbar to say
// so). Per CSS overflow, an `auto` on one axis computes the `visible` other
// axis to `auto` too, so the rounded corners still clip the header row's
// `bg-bg-2` exactly as before.
export const bkTableWrap = 'border border-border-0 rounded-2 overflow-x-auto bg-bg-1';

export const bkTable = 'w-full border-collapse text-sm text-text-1';

export const bkTh =
	'h-7 px-2.5 text-left text-xs font-medium tracking-[0.02em] text-text-3 bg-bg-2 border-b border-border-0 whitespace-nowrap';
export const bkThNum = `${bkTh} text-right`;

export const bkTr = 'group';

export const bkTd = 'h-[var(--table-row-h)] px-2.5 border-b border-border-0 group-hover:bg-bg-2';
export const bkTdDense =
	'h-[var(--table-row-h-dense)] px-2.5 border-b border-border-0 group-hover:bg-bg-2';
export const bkTdMono = 'font-mono text-[length:var(--mono-xs)] text-text-2';
export const bkTdNum = 'tabular text-right';

export const bkThSort =
	'inline-flex items-center gap-[3px] cursor-pointer appearance-none bg-transparent border-0 p-0 m-0 font-[inherit] text-inherit tracking-[inherit] hover:text-text-1';
export const bkSortArrow = 'text-[8px] text-accent-text';
