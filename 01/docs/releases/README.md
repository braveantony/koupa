# Release notes

這裡是 Koupa App 每次發布的 release notes：這一版改了什麼、推上 ghcr 的映像檔 digest 是哪一個。格式仿照 Cilium 在 GitHub 上的 release notes：標題 `# Koupa vX.Y.Z / vX'.Y.Z`、發布日期和一句話摘要；`## Summary of Changes` 只列有內容的分類（**Minor Changes**、**Bugfixes**、**CI Changes**、**Misc Changes**、**Other Changes**），每項一行，結尾寫 `(PR braveantony/koupa#編號, @作者)`；`## Docker Manifests` 底下 `### koupa (v1)`、`### koupa (v2)` 各一行 `映像檔:tag@sha256:digest`（發布後補上）；需要時再加一小段「升級方式」。分類標題保留英文方便對照，內文用繁體中文。一個版本一個檔案，v1、v2 兩條線同一次發布寫在同一個檔案（檔名用 v1 的版本號）。發布流程見[設計文件 6.2](../design-v2/06-reference.md#62-映像檔)。

- [v1.2.2 / v2.2.2](v1.2.2.md)
