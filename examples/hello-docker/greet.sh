#!/bin/sh
echo "Hello from Docker!"
echo "OS: $(uname -sr)"
echo "curl version: $(curl --version | head -1)"
