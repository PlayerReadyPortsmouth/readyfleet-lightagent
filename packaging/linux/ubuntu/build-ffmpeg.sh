#!/usr/bin/env bash
# Compile the engine-pinned FFmpeg source against Ubuntu 24.04's libva ABI.
# The latest downloaded binary dependency requires vaMapBuffer2, which 24.04
# does not provide. Do not hide that missing symbol or import newer glibc DSOs.
set -euo pipefail
engine=$1
prefix=/output/ffmpeg-ubuntu
build=/output/ffmpeg-ubuntu-build
source="$engine/third-party/build-deps/third-party/FFmpeg/FFmpeg"
[[ -f "$source/configure" ]] || { echo 'engine-pinned FFmpeg source is not initialized' >&2; exit 3; }
for patch_file in "$engine"/third-party/build-deps/patches/FFmpeg/FFmpeg/cbs/*.patch; do
  if patch --batch --dry-run -p1 -d "$source" < "$patch_file" >/dev/null; then
    patch --batch -p1 -d "$source" < "$patch_file"
  elif ! patch --batch --dry-run -R -p1 -d "$source" < "$patch_file" >/dev/null; then
    echo "CBS patch does not match pinned source: $patch_file" >&2; exit 3
  fi
done
mkdir -p "$build" "$prefix"
cd "$build"
"$source/configure" --prefix="$prefix" --cc=gcc-14 --cxx=g++-14 \
  --disable-all --disable-autodetect --disable-programs --disable-doc --disable-debug \
  --disable-iconv --enable-gpl --enable-static --enable-pic \
  --enable-avcodec --enable-avutil --enable-swscale --enable-swresample --enable-bsfs \
  --enable-libx264 --enable-libx265 --enable-vaapi --enable-libdrm \
  --enable-encoder=libx264,libx265,h264_vaapi,hevc_vaapi \
  --disable-vulkan --disable-cuda --disable-nvenc
make --jobs=2
make install
# Match the engine build-deps CBS target, including its pinned patches. These
# internal headers/symbols are required by Sunshine's bitstream code.
mkdir -p "$build/cbs"
objects=()
for file in libavcodec/cbs.c libavcodec/cbs_h2645.c libavcodec/cbs_av1.c libavcodec/cbs_vp8.c libavcodec/cbs_vp9.c libavcodec/cbs_mpeg2.c libavcodec/cbs_jpeg.c libavcodec/cbs_sei.c libavcodec/h264_levels.c libavcodec/h2645_parse.c libavcodec/vp8data.c libavutil/intmath.c; do
  obj="$build/cbs/$(basename "${file%.c}").o"
  gcc-14 -O2 -fPIC -I"$build" -I"$source" -Wno-incompatible-pointer-types -c "$source/$file" -o "$obj"
  objects+=("$obj")
done
ar rcs "$prefix/lib/libcbs.a" "${objects[@]}"
for tree in libavcodec libavutil; do
  while IFS= read -r -d '' header; do
    relative=${header#"$source/"}
    install -Dm0644 "$header" "$prefix/include/$relative"
  done < <(find "$source/$tree" -name '*.h' -type f -print0)
done
install -m0644 "$build/config.h" "$prefix/include/config.h"
# The engine's dependency list expects these when the software encoders exist.
cp /usr/lib/x86_64-linux-gnu/libx264.a /usr/lib/x86_64-linux-gnu/libx265.a "$prefix/lib/"
echo 'Ubuntu FFmpeg profile: H.264/HEVC software + VAAPI; no FFmpeg AV1/Vulkan/CUDA encoders.'
