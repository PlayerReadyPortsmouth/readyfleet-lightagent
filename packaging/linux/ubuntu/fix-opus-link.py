#!/usr/bin/env python3
"""Order the engine's static Opus dependency after its FFmpeg consumers."""
import pathlib
import sys

path = pathlib.Path(sys.argv[1]) / 'cmake/compile_definitions/common.cmake'
source = path.read_text()
before = '        ${Opus_LIBRARY}\n        ${FFMPEG_LIBRARIES}'
after = '        ${FFMPEG_LIBRARIES}\n        ${Opus_LIBRARY}'
if source.count(before) != 1:
    sys.exit('engine Opus/FFmpeg dependency layout changed; review the isolated build patch')
path.write_text(source.replace(before, after))

# Keep swresample after avcodec and before avutil for static FFmpeg builds.
ffmpeg = pathlib.Path(sys.argv[1]) / 'cmake/dependencies/ffmpeg.cmake'
text = ffmpeg.read_text()
needle = '        "${FFMPEG_PREPARED_BINARIES}/lib/libswscale.a"'
if text.count(needle) != 2:
    sys.exit('engine FFmpeg library layout changed; review the isolated build patch')
text = text.replace(needle, needle + '\n        "${FFMPEG_PREPARED_BINARIES}/lib/libswresample.a"')
ffmpeg.write_text(text)
