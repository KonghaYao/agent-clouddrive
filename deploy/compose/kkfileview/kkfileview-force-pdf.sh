#!/bin/sh
set -eu
# Deliberate, narrow change to checksum-pinned upstream v5.0.3:
# native spreadsheets otherwise ignore officePreviewType=pdf and become HTML.
# The private backend must produce PDF for the controlled raster adapter.
target="${1:-/src}/server/src/main/java/cn/keking/service/FileHandlerService.java"
test "$(grep -c 'boolean isHtmlView = suffix.equalsIgnoreCase' "$target")" = 1
sed -i 's/boolean isHtmlView = suffix.equalsIgnoreCase.*;/boolean isHtmlView = false; \/\/ CloudDrive: controlled raster requires PDF, never spreadsheet HTML./' "$target"
grep -q 'boolean isHtmlView = false;.*CloudDrive:' "$target"
