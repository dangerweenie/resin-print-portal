#!/bin/bash
set -e
echo "=== Deploying Printer Upload Portal ==="

BASE=/opt/printer-upload
mkdir -p $BASE/templates $BASE/files

cp app.py sliced_file_info.py pure_aes.py $BASE/
cp templates/*.html $BASE/templates/
cp ../usb-refresh.sh /usr/local/bin/usb-refresh.sh
chmod 755 /usr/local/bin/usb-refresh.sh

# Install deps if needed
pip3 install flask werkzeug gunicorn --break-system-packages -q

# Fix permissions
chown -R root:root $BASE
chmod -R 755 $BASE

# Reload and restart service
systemctl daemon-reload
systemctl restart printer-upload

echo "=== Done. ==="
echo "User portal:  http://<this-pi's-hostname>.lan/"
echo "Admin:        http://<this-pi's-hostname>.lan/admin"
echo "Admin login:  captain / <see /opt/printer-upload/.initial_admin_password on first run>"
