#!/usr/bin/env bash
# Regenerates the Ubuntu Sans woff2 subsets embedded by the theme package. This is not
# part of the build: the outputs are committed. Run it when the system fonts update or
# the character range changes. Needs uvx (fonttools) and the Ubuntu fonts package.
set -euo pipefail

src=/usr/share/fonts/truetype/ubuntu
out="$(cd "$(dirname "$0")/.." && pwd)/theme"
latin='U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+2000-206F,U+2074,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD'

subset() {
  uvx --from 'fonttools[woff]' pyftsubset "$src/$1" \
    --unicodes="$latin" \
    --layout-features='*' \
    --flavor=woff2 \
    --output-file="$out/$2"
  echo "wrote $out/$2"
}

subset 'UbuntuSans[wdth,wght].ttf' UbuntuSans.woff2
subset 'UbuntuSansMono[wght].ttf' UbuntuSansMono.woff2
