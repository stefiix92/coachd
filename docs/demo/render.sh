#!/bin/sh
# Renders the README walkthrough: index.html frame by frame with headless
# Chrome, then ../images/coachd-demo.gif. MP4=path also writes a 1080p MP4.
# Needs Chrome (set CHROME on Linux), ffmpeg and bc. To preview in a browser,
# open index.html#play; index.html#t=12.5 shows one moment.
set -eu
D=$(cd "$(dirname "$0")" && pwd)
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
FPS=${FPS:-15}
export CHROME FPS

if [ "${1:-}" = frame ]; then
	# render.sh frame TMP INDEX: one screenshot at INDEX/FPS seconds, in its
	# own Chrome profile (parallel runs can't share one), with one retry.
	tmp=$2 i=$3
	t=$(echo "scale=4; $i / $FPS" | bc)
	out=$tmp/frames/$(printf %05d "$i").png
	for _ in 1 2; do
		"$CHROME" --headless=new --disable-gpu --hide-scrollbars \
			--force-device-scale-factor=1.5 --window-size=1280,720 \
			--user-data-dir="$tmp/chrome-$i" --virtual-time-budget=400 \
			--screenshot="$out" "file://$D/index.html#t=$t" >/dev/null 2>&1 || true
		rm -rf "$tmp/chrome-$i"
		[ -s "$out" ] && exit 0
	done
	echo "render.sh: frame $i failed" >&2
	exit 1
fi

dur=$(sed -n 's/^const DURATION = \([0-9]*\);$/\1/p' "$D/index.html")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/frames"
n=$((FPS * dur))
seq 0 $((n - 1)) | xargs -P 8 -n 1 "$0" frame "$tmp"
[ "$(find "$tmp/frames" -name '*.png' | wc -l)" -eq "$n" ] || { echo "render.sh: missing frames" >&2; exit 1; }

ffmpeg -y -loglevel error -framerate "$FPS" -i "$tmp/frames/%05d.png" \
	-vf "fps=12,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle" \
	"$D/../images/coachd-demo.gif"
if [ -n "${MP4:-}" ]; then
	ffmpeg -y -loglevel error -framerate "$FPS" -i "$tmp/frames/%05d.png" \
		-c:v libx264 -pix_fmt yuv420p -crf 20 -preset slow -movflags +faststart "$MP4"
fi
