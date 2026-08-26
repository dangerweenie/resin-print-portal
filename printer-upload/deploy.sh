#!/bin/bash
set -e
echo "=== Deploying Printer Upload Portal ==="

BASE=/opt/printer-upload
mkdir -p $BASE/templates $BASE/files $BASE/tmp

cp app.py sliced_file_info.py pure_aes.py requirements.txt $BASE/
cp templates/*.html $BASE/templates/
cp ../usb-refresh.sh /usr/local/bin/usb-refresh.sh
chmod 755 /usr/local/bin/usb-refresh.sh

# Optional: a provisioning step (provision-sd.sh) that already knows the
# captain system password can stage it here so the app's first-run admin
# password matches it instead of generating an unrelated random one — see
# app.py's use of this file. Absent for a manual/redeploy run, which falls
# back to the existing random-password behavior.
[ -f .admin_password_seed ] && cp .admin_password_seed $BASE/.admin_password_seed

# Install deps into a dedicated venv (not system Python — avoids ever
# silently shifting an apt-managed package on this single-purpose Pi)
[ -d $BASE/venv ] || python3 -m venv $BASE/venv
$BASE/venv/bin/pip install -q -r $BASE/requirements.txt

# Fix permissions
chown -R root:root $BASE
chmod -R 755 $BASE
[ -f $BASE/.admin_password_seed ] && chmod 600 $BASE/.admin_password_seed

# Install the systemd unit itself — previously this was a manual one-time
# step described only in docs, so a from-scratch deploy silently had no
# service to restart. Committing the unit file here makes deploy.sh
# self-sufficient on both a first deploy and every redeploy after.
cp printer-upload.service /etc/systemd/system/printer-upload.service
systemctl daemon-reload
systemctl enable printer-upload
systemctl restart printer-upload

HOST="$(hostname).lan"
echo "=== Done. ==="
echo "User portal:  http://$HOST/"
echo "Admin:        http://$HOST/admin"
