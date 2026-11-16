FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates build-essential gcc-14 g++-14 cmake ninja-build pkg-config git \
    curl xz-utils zstd binutils python3-jinja2 python3-setuptools nodejs npm \
    libcap-dev libcurl4-openssl-dev libdrm-dev libevdev-dev libgbm-dev \
    libminiupnpc-dev libnuma-dev libopus-dev libpipewire-0.3-dev libpulse-dev \
    libssl-dev libsystemd-dev libudev-dev libwayland-dev libx11-dev libxcb-shm0-dev \
    libxcb-xfixes0-dev libxcb1-dev libxfixes-dev libxrandr-dev libxtst-dev \
    libvulkan-dev glslang-tools libva-dev libnotify-dev && \
    rm -rf /var/lib/apt/lists/*
ENV CC=gcc-14 CXX=g++-14
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev && \
    rm -rf /var/lib/apt/lists/*
