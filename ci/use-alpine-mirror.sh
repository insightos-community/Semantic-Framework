#!/bin/sh
set -eu

# Alpine 官方镜像没有 bash。发布 Job 必须用 sh。
mirror="${ALPINE_MIRROR:-https://mirrors.aliyun.com/alpine}"
if [ -f /etc/apk/repositories ]; then
  sed -i "s#https\\?://dl-cdn.alpinelinux.org/alpine#${mirror}#g" /etc/apk/repositories
fi
