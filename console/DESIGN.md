# Console design rules

The console should feel at home to Tailscale, Cloudflare Tunnel, NetBird and ngrok users: quiet,
fast, obvious. These rules come from product review; follow them for every new screen.

## Words

- The product object is a **machine** (as in the API and the Go code), never "device".
- No filler: no taglines, no explanatory paragraphs, no decorative badges or pills. A shadcn
  `Badge` (`variant="outline"`, muted text, normal weight) is fine for a short state label on a
  list item, like "Default". A short hint under a field
  is fine when it prevents a mistake ("Lowercase letters, numbers and dashes. Up to 63.").
- Mark optional fields with a muted "Optional" on the right of the label.

## Brand

The brand guidelines live in the Paper file "Intuitive zephyr", page "Brand guidelines" (status in
`../paper.md`). In code:

- Mark on a 10 × 6 cell grid; render at heights divisible by 6 px (12, 18, 24, …).
- Lockup: wordmark Figtree SemiBold, lowercase, −0.025em, font-size = mark height, gap ⅔ of it
  (sidebar: 18 px mark and text, 12 px gap). Reversed (white on ink or on the blue brand bar) the
  wordmark is Medium 500, so it reads the same weight as SemiBold 600 on light grounds.
- Brand gradient top → bottom: #4275F8 0 %, #1D52EB 55 %, #0F3DD8 100 %. Boat fill #FFFFFF → #EAF0FF.

## Theme and components

- Use shadcn/ui (Base UI, `base-mira`) components as the registry ships them. Change look through
  the tokens in `app/globals.css`, not by restyling components. Check with
  `bunx shadcn add <name> --diff` before editing a component in `components/ui`.
- Neutrals are pure greys; blue (the logo's mid blue, `--brand`) is only for primary actions and
  focus. No gradients on controls. The one gradient is the sidebar's brand bar (the logo
  gradient, with the boat animation on page load; `.brand-bar` in globals.css).
- Panels are filled (`surface` utility), not outlined. Lines inside lists use `border-divider`.
- Large display panels (stat cards, edge tiles) use `surface-pattern`: the same fill with faint
  diagonal stripes drifting slowly (`--surface-pattern`, ΔL* 1.0 per theme, still under reduced
  motion). Controls, inputs and panels holding
  forms or lists stay plain `surface`.
- Numbers are tabular everywhere (global rule).
- Icons: Phosphor. `regular` by default, `fill` for the selected item. Server-imported modules use
  `@phosphor-icons/react/dist/ssr`.

## Aligning things with text

Icons, status dots and badges beside text line up with the **letters**, not the line box: the
line box's spacing differs by font and browser (Safari sits about 1px off Chrome), so plain
`items-center` drifts.

- In a centered row, give the text beside the item `trim-cap` (globals.css: trims its box to cap
  height and baseline), and keep `items-center` on the row.
- A trimmed text that can overflow uses `truncate-x`, not `truncate`; `overflow: hidden` would cut
  off descenders that now hang outside its box.
- A row holding trimmed text gets a fixed height, `h-[1lh]`, and every text in it is trimmed.
  Otherwise an optional extra (a status word, a badge) changes the row's height, and the same
  row no longer lines up across cards or list items that lack it.
- Rows of text in different sizes (a label and a big number) align with `items-baseline`. An icon
  inside such a label gets `self-center` in an `inline-flex items-baseline` wrapper, so the
  label's baseline comes from its text: an icon's own baseline is its bottom edge.
- Never nudge with `translate`/`top` offsets.
- Badges and buttons are shadcn components; put `trim-cap` on the `Badge` itself when it sits
  next to text, so its own label is centered the same way.

## Dialogs

Model every dialog on **Add machine** (`components/add-machine-dialog.tsx`):

- `DialogContent` with `className="gap-4 sm:max-w-md"`, a `DialogTitle` only (no description line).
- Lead with the thing the user came for (the command, the choice), then options below a
  `border-t border-divider` rule.
- Fields: `Field` with `gap-1.5`, `FieldLabel` (+ "Optional" when optional), the control, then
  either `FieldError` or one muted `text-xs` hint.
- Results and status sit directly under the thing they describe (status line under the command),
  not in a footer.
- Footer buttons (`DialogFooter`, Cancel + primary, `size="lg"`) only for forms that commit on
  submit. Destructive confirmations use `AlertDialog` with a `destructive` action.
- Mount dialog bodies only while open, so each opening starts from current data.

An object's own fields are edited **in place** on its page: hovering the title shows a pencil;
the title and description become inputs with the same type and position, only a faint dashed
underline, small ✓ / ✕ icon buttons, Enter to save, Escape to cancel, and a hint line only when
something needs saying. From lists, the same edit opens as a dialog ("Edit…" in the row menu),
sharing one save and validation hook. Removal sits in a **Danger zone** at the end of the
object's page and in the row menu; both open the same confirmation that asks you to type the
object's name.

## Destructive actions

- A Danger zone is a faint red panel (`bg-destructive/5`, `dark:bg-destructive/10`), no border.
- Its button is a light chip with red text: shadcn `secondary` with `bg-background` in light mode
  and `dark:bg-white/10` in dark, so it sits lighter than the red panel
  and opens the confirmation; the confirm button is shadcn's `destructive` variant as shipped.

## Lists and menus

- Tables: no outer box, faint row dividers, status as a dot after the name with a tooltip.
- A row's "…" menu holds only actions the row cannot do itself. Anything reachable by clicking
  the row, a tab or an inline control does not belong in it. Exception: copying the row's
  identifiers (hostname, IP), so right-clicking anywhere on the row reaches them.
- Anything copyable copies when its value is clicked; the copy icon shows the result.

## Navigation

- The sidebar is global ("All machines"). A machine page has tabs for the same sections scoped to
  that machine; each tab is a route under `/machines/<id>`.
- Pickers that can grow (machines) show a short smart list and search the server when typing.
- Every breadcrumb picker (workspace, machine) is built the same way: `CrumbPickerTrigger`
  (`components/crumb-picker.tsx`; the crumb brightens on hover and while open, no box), a
  popover with a search field, rows with the name on the left and one muted detail on the right
  (IP, role), and the current item in bold instead of a check.
