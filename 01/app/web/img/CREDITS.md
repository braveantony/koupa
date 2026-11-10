# 飲料照片來源

| 檔案 | 品項（menu id） |
|---|---|
| `boba-milk-tea.webp` | 1 波霸奶茶 |
| `sunset-black-tea.webp` | 2 晚霞紅茶 |
| `green-tea-trio.webp` | 3 綠茶三劍客 |
| `winter-melon-lemon.webp` | 4 冬瓜檸檬 |

- 產生方式：OpenAI Codex CLI 0.163.0 內建的 image_gen 工具（工具沒有揭露使用的模型），2026-11-10。
- 提示詞：四張共用下面這段風格描述，前面再加上各自的飲料描述。綠茶三劍客第一版太黃，重抽時加上 "clearly green, not yellow"，並要求比照另外三張的杯子、吸管和光線。
  - 共用：`Studio product photo of a single Taiwanese bubble-tea style drink in a plain clear plastic cup with a sealed film lid and a thick straw, centered, 3/4 front view, on a warm cream background (#F7F1E8), soft natural window light from the left, gentle shadow, shallow depth of field, photorealistic, high detail, square 1:1, no text, no logo, no hands, no other objects.`
  - 波霸奶茶：`Creamy beige milk tea with large dark brown tapioca pearls (boba) settled at the bottom, light ice.`
  - 晚霞紅茶：`Clear amber-red black tea with ice cubes, glowing like a sunset when backlit, slight gradient from deep red at the bottom to golden at the top.`
  - 綠茶三劍客：`Pale jade-green jasmine green tea — clearly green, not yellow — with translucent nata de coco cubes at the bottom and two thin lemon slices, ice cubes.`
  - 冬瓜檸檬：`Warm amber winter melon tea with lemon slices and ice cubes, slightly cloudy, refreshing.`
- 後製：原圖為 1254×1254 PNG，用容器 `docker.io/library/alpine:3.20.3` 裡的 `cwebp`（libwebp 1.3.2）縮成 800×800 WebP，品質 80，去掉 metadata：
  `cwebp -q 80 -m 6 -resize 800 800 -metadata none <in>.png -o <out>.webp`
- 使用權：依 OpenAI 的使用條款，使用者擁有工具產生的輸出內容，可以自行使用（<https://openai.com/policies/terms-of-use>）。
