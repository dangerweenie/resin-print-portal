#!/bin/bash
set -e
echo "=== Deploying Printer Upload Portal ==="

BASE=/opt/printer-upload
mkdir -p $BASE/templates $BASE/files $BASE/tmp

cp app.py sliced_file_info.py pure_aes.py requirements.txt $BASE/
cp templates/*.html $BASE/templates/
cp ../usb-refresh.sh /usr/local/bin/usb-refresh.sh
chmod 755 /usr/local/bin/usb-refresh.sh

# Install deps into a dedicated venv (not system Python — avoids ever
# silently shifting an apt-managed package on this single-purpose Pi)
[ -d $BASE/venv ] || python3 -m venv $BASE/venv
$BASE/venv/bin/pip install -q -r $BASE/requirements.txt

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
