# Surface: storefront (點餐 / 廚房 / 叫號看板)

Mode: Operate. Pages: `01/app/web/order.html`, `kitchen.html`, `board.html`, shared `app.css`. Shipped system: [DESIGN.md](../../DESIGN.md).

## Direction contract

- **THESIS**: the shop's own consumables are the interface: the printed sealing film on every cup and the label-printer sticker on its side. Refuses the generic card-grid shop template.
- **OWN-WORLD**: cool white/blue-grey ground `#F4F6FB`; cobalt film `#1E46B4` with a repeating dot print (white + golden-yellow `#F2C230` dots) used as a field (header band, the board's big film disc), never as scattered decoration and never under text; label stickers: white, small radius, 1px drop line, slight tilt (≤2°, the called number 3°), label-printer feel from monospace only on digit-free text (slogan, notices); numbers in a bold sans with tabular lining figures (a dotted zero reads as a decimal point); ink `#10182E`; red `#B42318` only for 售完 and errors. Round shapes for anything that is "the cup", rectangular stickers for anything that is "information".
- **STORY**: a customer picks cups (round film discs with a sticker each), sees `N 杯 / NT$ X` on the bottom sticker bar and gets a ticket number printed big on a sticker over a film disc; the kitchen works through printed label strips and presses 完成; the shop screen presses the called number onto the big film disc.
- **FIRST VIEWPORT**: order page on a 390px phone: film header band with the tilted brand sticker and nav pills, 今天想喝什麼？, the first row of two cup discs with their label stickers and steppers, the sticky cart sticker bar. Board on 16:9: the same (shorter) film header band, 製作中 label strips on the left, the big film disc with 請 / number / 號取餐 on the right.
- **FORM**: signature move = the called number on a tilted white sticker pressed onto the cobalt dotted film disc. `seed: none (pinned)` because the user picked direction B from four previews.
- **RAISES**: none.
