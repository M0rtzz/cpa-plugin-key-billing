# Vendored browser dependency

SheetJS Community Edition 0.20.3 is used only to create administrator Excel
exports. It is served from the plugin's own resource route, without a runtime
CDN dependency. The unmodified upstream distribution and Apache 2.0 license
were downloaded from:

- https://cdn.sheetjs.com/xlsx-0.20.3/package/dist/xlsx.full.min.js
- https://cdn.sheetjs.com/xlsx-0.20.3/package/LICENSE

Update the pinned filename, resource handler, deployment documentation and
browser workbook regression checks together when upgrading this dependency.
