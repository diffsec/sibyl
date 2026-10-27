# Show the Sibyl live menu once, on the first console, for root.
if [ "$(id -u)" = 0 ] && [ "$(tty)" = /dev/tty1 ] && [ -z "${SIBYL_MENU_SHOWN:-}" ]; then
    export SIBYL_MENU_SHOWN=1
    /usr/libexec/sibyl/live-menu
fi
