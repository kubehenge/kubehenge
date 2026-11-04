#!/bin/sh
# Rebuilds the home page's web fonts from the full fonts, keeping only the characters the home page
# can draw (ASCII, and whatever else HengeHome.vue, henge.js and home.css contain):
#
#   website/subset-fonts.sh
#
# Run it after the home page gains a character outside those, or after @fontsource's fonts move.
# fonttools and brotli come from fonts-source/requirements.txt, whose hashes uv checks. Each font is
# under the SIL Open Font License 1.1, which allows subsetting. None names a Reserved Font Name, so
# the files keep their names and name tables. The licences sit beside the fonts.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
theme="${here}/.vitepress/theme"
fontsource="${here}/node_modules/@fontsource"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Every character of the three sources, plus printable ASCII.
cat "${theme}/HengeHome.vue" "${theme}/henge.js" "${theme}/home.css" > "${work}/text"
printf '%s' ' !"#$%&'"'"'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\]^_`abcdefghijklmnopqrstuvwxyz{|}~' >> "${work}/text"

subset() {
    uv run --no-project --quiet --with-requirements "${here}/fonts-source/requirements.txt" \
        pyftsubset "$1" --output-file="${theme}/fonts/$2.woff2" --flavor=woff2 \
        --text-file="${work}/text" --layout-features='*' --name-IDs='*'
}

subset "${here}/fonts-source/JetBrainsMono-Regular.woff2" JetBrainsMono-Regular
subset "${here}/fonts-source/JetBrainsMono-Bold.woff2" JetBrainsMono-Bold
subset "${fontsource}/atkinson-hyperlegible-next/files/atkinson-hyperlegible-next-latin-400-normal.woff2" AtkinsonHyperlegibleNext-400
subset "${fontsource}/atkinson-hyperlegible-next/files/atkinson-hyperlegible-next-latin-700-normal.woff2" AtkinsonHyperlegibleNext-700
subset "${fontsource}/big-shoulders-display/files/big-shoulders-display-latin-800-normal.woff2" BigShouldersDisplay-800
subset "${fontsource}/big-shoulders-display/files/big-shoulders-display-latin-900-normal.woff2" BigShouldersDisplay-900
