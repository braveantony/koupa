# 飲料照片來源

| 檔案 | 品項（menu id） |
|---|---|
| `boba-milk-tea.webp` | 1 波霸奶茶 |
| `sunset-black-tea.webp` | 2 晚霞紅茶 |
| `green-tea-trio.webp` | 3 綠茶三劍客 |
| `winter-melon-lemon.webp` | 4 冬瓜檸檬 |

- 產生方式：OpenAI Codex CLI 0.163.0 內建的 image_gen 工具（工具沒有揭露使用的模型），2026-11-10。
- 提示詞摘要：手搖飲商品照，一杯透明塑膠杯裝的冰飲（插吸管、杯蓋），米色背景、窗邊斜射的暖色自然光，正方形構圖、不含文字與商標。四張分別是：加黑色珍珠的奶茶、上淺下深的琥珀色冰紅茶、加檸檬片的綠茶、加檸檬片的冬瓜茶。
- 後製：原圖為 1254×1254 PNG，用容器 `docker.io/library/alpine:3.20.3` 裡的 `cwebp`（libwebp 1.3.2）縮成 800×800 WebP，品質 80，去掉 metadata：
  `cwebp -q 80 -m 6 -resize 800 800 -metadata none <in>.png -o <out>.webp`
- 使用權：依 OpenAI 的使用條款，使用者擁有工具產生的輸出內容，可以自行使用（<https://openai.com/policies/terms-of-use>）。
