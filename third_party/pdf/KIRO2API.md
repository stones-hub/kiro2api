# 本地副本说明

来源：github.com/ledongthuc/pdf @ 6c8c28e0e8a0（2026-09-07），只保留非测试源码，许可证见 LICENSE。

唯一改动：`page.go` 的 `getEncoder` 优先使用字体的 ToUnicode CMap。原版在字体同时带
Encoding 字典时忽略 ToUnicode，macOS 生成的中文 PDF 会提取成乱码。

由 `go.mod` 的 `replace github.com/ledongthuc/pdf => ./third_party/pdf` 引用。
