# The podman sandbox plus py-desmume and a ROM dump, for repos whose checks run DeSmuME (rust-mm).
# Debian, not Alpine: py-desmume ships only glibc wheels. The ROM comes from its own build context,
# so it never enters this repo:
#   podman build -t agent-sm-pi -f sandbox/desmume.Containerfile --build-context rom=C:\path\to\rom-dir --build-context saves=C:\path\to\rescue-re\saves sandbox
# The maintainer saves come from their own context too, so they never enter this repo either.
# Inside: ~/venvs/desmume/bin/python (imports as `desmume`) and the dump at /rom/pmdb.nds.
FROM docker.io/library/node:22-bookworm-slim

ARG PI_VERSION=1.6.0
ARG ROM=pmdb.nds

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates python3 python3-venv \
    libglib2.0-0 libgl1 libsdl2-2.0-0 \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /root/venvs/desmume \
 && /root/venvs/desmume/bin/pip install --no-cache-dir --only-binary=:all: py-desmume \
 && npm install -g --omit=dev "@earendil-works/pi-coding-agent@${PI_VERSION}" \
 && npm cache clean --force \
 && rm -rf /root/.npm /tmp/*

COPY --from=rom ${ROM} /rom/pmdb.nds
COPY --from=saves . /root/rescue-re/saves/
ENV SDL_VIDEODRIVER=dummy SDL_AUDIODRIVER=dummy PMDB_ROM=/rom/pmdb.nds

WORKDIR /work
