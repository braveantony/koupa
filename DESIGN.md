# Design

The shipped visual system of the Koupa 酷杯 pages (`01/app/web/`: `order.html`, `kitchen.html`, `board.html`, one `app.css`). Product truth and constraints are in [PRODUCT.md](PRODUCT.md); the direction contract is in [.impeccable/surfaces/storefront.md](.impeccable/surfaces/storefront.md).

## World: sealing film and label sticker

The shop's own consumables are the interface. Every cup is sealed with a printed cobalt film, and the label printer puts a small white sticker on its side. Two rules follow:

- **Round = the cup.** Anything that stands for a drink or a cup is a circle: the menu photo in a film rim, the confirmation disc, the board's big disc, the small cup mark in 品項供應.
- **Rectangle = information.** Anything you read is a white label sticker: name/price/stepper, order strips, the cart bar, the receipt, the called number.

## Palette

| Token | Hex | Role |
|---|---|---|
| `--ground` | `#F4F6FB` | Page background (cool white, blue-grey) |
| `--surface` | `#FFFFFF` | Stickers, the board header |
| `--ink` | `#10182E` | Text (17.6:1 on white) |
| `--ink-2` | `#4B5570` | Secondary text: prices, statuses, hints (7.4:1 on white) |
| `--line` | `#D5DAE6` | Hairlines, dashed label rules, disc outline |
| `--film` / `--primary` | `#1E46B4` | The sealing film; primary buttons (white text 8.1:1) |
| `--film-deep` | `#163889` | Hover on film-coloured buttons |
| `--dot` | `#F2C230` | Yellow film dots; current nav pill; count pill (ink text 10.5:1) |
| `--soft` | `#E8ECF5` | Disabled button fill, light hover |
| `--sold-disc` | `#C7CCD8` | Sold-out cup rim, board disc outline |
| `--red` | `#B42318` | Only 今日售完 and errors (6.6:1 on white) |
| `--red-bg` / `--red-line` | `#FDECEA` / `#F1B5AE` | Error notice fill and border |

No yellow dot ever sits under text: text on film is always on a solid sticker or pill.

## Type

- Text: system CJK sans, `"PingFang TC", "Noto Sans TC", "Noto Sans CJK TC", "Microsoft JhengHei", "Heiti TC", "WenQuanYi Zen Hei", system-ui, sans-serif`. Names and headings 900, body 400.
- Label-printer text (numbers, prices, quantities, statuses, the slogan): `ui-monospace, "SFMono-Regular", Menlo, Consolas, "Liberation Mono", "Noto Sans Mono", monospace` with `tabular-nums`, so ticket digits stay aligned whatever monospace is installed. The class `.num` applies it.
- No web fonts and no external requests (enforced by `web_test.go`).
- Scale: 13–15 small, 17 names/body, 20–22 headings, 46 kitchen number, `clamp(56px, 18vw, 88px)` confirmation number, up to 140px board number (sized by `vw`/`vh`).

## Components

- **Film field** (`.film`): `#1E46B4` with a repeating dot print (white and yellow dots, `--tile` 48px / `--r` 5px; 44px on the confirmation disc, 64px / 7px on the board). Used only as a field: the header band on 點餐 and 廚房, the confirmation disc, the board's disc.
- **Sticker** (`.sticker`): white, radius 4px (6px for the big ones), drop line `0 1px 0 rgba(16,24,46,.22), 0 3px 8px rgba(16,24,46,.08)`. Lists tilt by at most 1.5deg (menu labels -1.5 / 1 / 1.5 / -1deg, kitchen strips ±0.6deg); the called number tilts -3deg.
- **Header**: film band with the brand sticker (Koupa 酷杯 + slogan in mono, -1.5deg) and nav pills (white 94%, current = yellow). The board header is plain white with a bottom hairline so the disc is the only film on screen.
- **Menu cup** (`.card`): photo in a circle with a printed film rim (`repeating-conic-gradient` cobalt/white, 7px), label sticker below with name, `NT$` price, then the stepper: 44px round − / + with drawn marks (CSS bars, no glyphs). The quantity is mono 20px.
- **Cart bar**: sticky white sticker at the bottom, `N 杯 / NT$ X` in mono, pill button 確認點餐 (48px).
- **Confirmation**: 訂單成立, a film disc with a white rim holding the 您的號碼牌 sticker (number in mono), the 叫號 note, a receipt sticker (mono lines, dashed rule above 合計), outline pill 再點一單.
- **Kitchen strip** (`.ticket-card`): 號碼牌 + number 46px mono, `NT$` total, items between dashed rules, full-width 完成 button 56px.
- **Supply sticker**: small cup mark (film blue / grey), name, 供應中 or 今日售完, outline pill toggle (filled when sold out: 恢復供應).
- **Board**: left 製作中 = label strips in a 2-column grid (number + 準備中), max 16 including a 還有 N 張 strip; right = big disc (`min(100%, 64vh, 520px)`, white rim) with the sticker 請 / number / 號取餐, 剛剛叫過 row of small stickers below. The 請取餐 heading is visually hidden.

## States

- Sold out: grey cup rim, greyscale photo at 60%, name in `--ink-2`, 今日售完 in red, both stepper buttons disabled.
- Disabled buttons: `--soft` fill with `--ink-2` text; disabled steppers keep a white face with a `--line` border.
- Pending/unknown order: steppers locked (`#menu.locked`, 70% opacity), notice as a red error sticker; 送出中… as a white mono sticker.
- Empty: centred `--ink-2` line (目前沒有製作中的訂單); the board disc shows a 等待叫號… sticker.
- Focus: 3px `--film` outline, offset 2px; white outline on the film header.
- Browser surfaces: selection = yellow with ink text, caret = film, scrollbar thumb `#9AA3BA` on ground.

## Motion

One moment: `press-on`, a sticker pressed onto the film (from -7deg / 1.08 scale with a lifted shadow to -3deg / 1, 320ms, exponential ease-out; always visible). It plays when the confirmation appears and on each new called number. `prefers-reduced-motion: reduce` turns it off.

## Responsive

- 360px and up with no horizontal scroll. The menu grid is `repeat(auto-fill, minmax(148px, 1fr))` inside 46rem: 2 cups per row on phones, 4 on desktop.
- Nav wraps to its own row below 600px.
- Board: two columns (2fr / 3fr) sized by `vh` for 16:9 screens; below 720px one column with the disc first (`min(100%, 360px)`) and the strips below.
- Touch targets ≥44px (steppers, nav, toggles), 48px for 確認點餐 / 再點一單, 56px for 完成.
