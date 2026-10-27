# laptop-backup

บริการเล็ก ๆ บนโน้ตบุ๊ก (Linux) ที่ **สำรองโฟลเดอร์ที่เลือกขึ้น Google Drive แบบ mirror 1:1**
พร้อมหน้าเว็บสำหรับตั้งค่าที่ `http://127.0.0.1:8095` (หรือ `http://backup` ถ้าตั้ง nginx proxy ไว้ ดูหัวข้อติดตั้ง)

- ไฟล์ใหม่ → อัปโหลด
- ไฟล์ที่แก้ไข → อัปเดตไฟล์เดิมบน Drive (file ID เดิม ไม่มีสำเนาซ้ำ)
  ถ้าเปลี่ยนแค่เวลาแก้ไข จะเทียบ md5 กับ Drive ก่อน ไม่อัปโหลดซ้ำโดยไม่จำเป็น
- ไฟล์/โฟลเดอร์ที่ลบในเครื่อง → ย้ายไป **ถังขยะ** ของ Drive (ไม่ลบถาวร กู้คืนได้ ~30 วัน)
- นำโฟลเดอร์ออกจากรายการ → สำเนาบน Drive ยังอยู่ ไม่ถูกแตะ
- โฟลเดอร์ต้นทางหายไป (เช่น ดิสก์ไม่ได้ mount) → ข้ามไป ไม่ลบอะไรบน Drive
- ตารางเวลา: ทุก N ชั่วโมง / ทุกวันเวลา HH:MM / หยุดชั่วคราว
  ถ้าเครื่องปิดหรือหลับตอนถึงเวลา จะสำรองให้ภายใน ~1 นาทีหลังบริการเริ่ม
  และมีปุ่ม “สำรองข้อมูลตอนนี้”

โครงสร้างบน Drive: ทุกโฟลเดอร์อยู่ใต้โฟลเดอร์หลักที่คุณเลือก (ใส่ Folder ID ในหน้า **ตั้งค่า**
ก่อนสำรองครั้งแรก — ไม่มีค่าเริ่มต้น และจะไม่สำรองจนกว่าจะตั้ง) ตาม path เทียบกับ `$HOME` เช่น `~/Documents/Personal` → `<โฟลเดอร์หลัก>/Documents/Personal`
ส่วน path นอก `$HOME` จะอยู่ใต้ `<โฟลเดอร์หลัก>/_root/...`

## ทำงานอย่างไร

- โปรแกรม Go ไฟล์เดียว (static binary, ไม่ต้องใช้ cgo) ฟังที่ `127.0.0.1:8095` เท่านั้น
- รันเป็น **systemd user service** (ทำงานเฉพาะตอน login อยู่ ซึ่งพอสำหรับการสำรอง
  จึง **ไม่ต้อง** `loginctl enable-linger`)
- เก็บดัชนีไฟล์ (path → Drive file ID) และประวัติใน SQLite ทำให้แต่ละรอบอัปโหลดเฉพาะที่เปลี่ยน
- ไฟล์ใหญ่กว่า 5 MB ใช้ resumable upload และสตรีมจากดิสก์ (ไม่โหลดทั้งไฟล์เข้าหน่วยความจำ)
  เจอ 429/5xx จะลองใหม่แบบ exponential backoff ไฟล์ที่ล้มเหลวจะถูกบันทึกและข้ามไป ไม่หยุดทั้งรอบ
- ข้าม symlink, socket, FIFO และไฟล์ที่อ่านไม่ได้ (บันทึกใน log) — ไฟล์ที่อ่านไม่ได้จะไม่ถูกลบจาก Drive
- ขณะว่าง CPU ~0 (หลับรอ timer) ใช้หน่วยความจำราว 20 MB

| ไฟล์ | ที่อยู่ |
|---|---|
| ตั้งค่า | `~/.config/laptop-backup/config.json` |
| OAuth client | `~/.config/laptop-backup/credentials.json` (0600) |
| refresh token | `~/.config/laptop-backup/token.json` (0600) |
| ดัชนี/ประวัติ | `~/.local/share/laptop-backup/state.db` |
| log | `~/.local/share/laptop-backup/laptop-backup.log` (และ `journalctl --user -u laptop-backup`) |

## ติดตั้ง

ต้องมีอย่างใดอย่างหนึ่ง: **Go ≥ 1.26** หรือ **Docker** (สคริปต์จะ build ใน container `golang:1.26` ให้เอง
ถ้าไม่มี Go) หรือไฟล์ `bin/laptop-backup` ที่ build มาจากเครื่องอื่นแล้ว (`make build`)
และต้องมี OAuth client ของ Google เป็นของตัวเอง (ดูหัวข้อถัดไป)

```bash
git clone <repo> ~/laptop-backup && cd ~/laptop-backup
cp install.local.env.example install.local.env   # ไม่บังคับ: แก้ค่าที่ต้องการ
./install.sh
```

`install.sh` ทำทีละขั้นและรันซ้ำได้ (ขั้นที่ทำแล้วหรือไม่ได้ตั้งค่าไว้จะขึ้น `[skip]`):

1. build แล้วติดตั้งเป็น `~/.local/bin/laptop-backup`
2. เขียน `config.json` ค่าเริ่มต้น (ถ้ายังไม่มี)
3. สร้าง `credentials.json` — ถ้าตั้ง `IMPORT_ENV` เป็นไฟล์ `.env` ที่มี
   `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` จะถามว่าจะนำเข้าไหม (อ่านแค่สองค่านี้ ไม่นำ refresh token มา)
   ถ้าไม่ได้ตั้งจะให้พิมพ์เอง หรือจะวางไฟล์ JSON ที่ดาวน์โหลดจาก Google Cloud Console
   ไว้ที่ `~/.config/laptop-backup/credentials.json` เองก็ได้
4. ติดตั้ง `deploy/laptop-backup.service` ไปที่ `~/.config/systemd/user/` แล้ว `enable` + `restart`
5. (ถ้าตั้ง `PROXY_CONF_DIR`) เขียน `deploy/backup.conf` (ใส่ `server_name` เป็น `PROXY_HOST`) ลงใน
   conf.d ของ nginx แล้ว `docker exec <PROXY_CONTAINER> nginx -s reload`
6. (ถ้าตั้ง `PROXY_CONF_DIR`) เพิ่ม `127.0.0.1   <PROXY_HOST>` ใน `/etc/hosts` (ใช้ sudo เฉพาะเมื่อยังไม่มี)

ถ้าไม่ได้ตั้ง proxy ก็ใช้งานผ่าน `http://127.0.0.1:8095` ได้เลย

### ตั้งค่าของ install.sh

ค่าทั้งหมดไม่บังคับ ลำดับความสำคัญ: ตัวแปร environment > ไฟล์ `install.local.env` (อยู่ข้าง `install.sh`,
ไม่ถูก commit) > ค่าเริ่มต้น ไฟล์นี้อ่านเป็นข้อความ `KEY=value` ธรรมดา (ไม่ได้ `source`)
ดูตัวอย่างทุกค่าใน [`install.local.env.example`](install.local.env.example)
และตรวจค่าที่จะใช้จริงได้ด้วย `./install.sh --print-config`

| ตัวแปร | ค่าเริ่มต้น | ความหมาย |
|---|---|---|
| `PROXY_CONF_DIR` | (ว่าง = ข้ามขั้น 5–6) | โฟลเดอร์ `conf.d` ของ nginx reverse proxy ในเครื่อง |
| `PROXY_CONTAINER` | `local-proxy` | ชื่อ Docker container ของ nginx นั้น |
| `PROXY_HOST` | `backup` | ชื่อโฮสต์ที่ proxy ให้บริการ (ถ้าไม่ใช่ `backup` จะเขียน systemd drop-in ให้บริการรับชื่อนี้) |
| `IMPORT_ENV` | (ว่าง = ถาม) | ไฟล์ `.env` ที่จะนำเข้า `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` |
| `GO_IMAGE` | `golang:1.26` | image ที่ใช้ build เมื่อไม่มี Go |

## สร้าง OAuth client ของ Google (ครั้งแรกครั้งเดียว)

โปรแกรมนี้ไม่มี client id/secret ติดมาด้วย ต้องสร้างเองใน [Google Cloud Console](https://console.cloud.google.com/):

1. สร้างหรือเลือกโปรเจกต์ แล้วไปที่ **APIs & Services → Library** เปิดใช้ **Google Drive API**
2. **APIs & Services → OAuth consent screen** (Google Auth Platform): ตั้งชื่อแอป เลือก User type
   **External** แล้วใส่อีเมลของตัวเอง
   - กด **Publish app** ให้สถานะเป็น **In production** — ถ้าค้างไว้ที่ *Testing*
     refresh token จะหมดอายุทุก 7 วันและต้องกดเชื่อมต่อใหม่ทุกสัปดาห์
   - แอปที่ใช้เองไม่ต้องส่งให้ Google ตรวจสอบ ตอนอนุญาตครั้งแรกจะเห็นหน้า “Google hasn't verified this app”
     ให้กด **Advanced → Go to … (unsafe)**
3. **APIs & Services → Credentials → Create credentials → OAuth client ID** เลือกประเภท **Desktop app**
   (redirect `http://127.0.0.1:8095/oauth/callback` ใช้ได้กับ Desktop client โดยไม่ต้องลงทะเบียน)
4. คัดลอก **Client ID** และ **Client secret** ไปใส่ตอน `install.sh` ถาม หรือดาวน์โหลด JSON
   แล้วบันทึกเป็น `~/.config/laptop-backup/credentials.json` (`chmod 600`)

โปรแกรมขอสิทธิ์ scope `https://www.googleapis.com/auth/drive` (ต้องใช้สิทธิ์เต็มเพื่ออัปเดตและย้ายไฟล์
ที่มีอยู่แล้วในโฟลเดอร์ปลายทาง) ใช้ OAuth แบบผู้ใช้ ไม่ใช่ service account เพราะ service account
ไม่มีพื้นที่เก็บไฟล์ใน Drive ส่วนตัว

## เชื่อมต่อบัญชี Google และเลือกโฟลเดอร์ปลายทาง

1. เปิดหน้าเว็บ → **ตั้งค่า** → **เชื่อมต่อบัญชี Google**
2. เลือกบัญชีและกดอนุญาต (สิทธิ์ `drive`) — Google จะส่งกลับมาที่
   `http://127.0.0.1:8095/oauth/callback` (ต้องเปิดเบราว์เซอร์บนโน้ตบุ๊กเครื่องนี้)
3. สร้างโฟลเดอร์บน Google Drive ที่จะเก็บสำเนา แล้วเปิดโฟลเดอร์นั้นในเบราว์เซอร์ URL จะเป็น
   `https://drive.google.com/drive/folders/<FOLDER_ID>` (อาจมี `/u/0/` หรือ `?...` ต่อท้าย)
   คัดลอกเฉพาะส่วน `<FOLDER_ID>` (ตัวอักษร ตัวเลข `-` `_` ยาวราว 33 ตัว) ไปใส่ช่อง **Folder ID**
   ในหน้า **ตั้งค่า** → กด **ทดสอบการเข้าถึง** → **บันทึก**
   (ระหว่างที่ยังไม่ได้ตั้ง หน้าภาพรวมจะแจ้งเตือน และจะไม่สำรองข้อมูล)
4. ไปที่ **โฟลเดอร์** → เลือกโฟลเดอร์ที่จะสำรอง → กลับหน้า **ภาพรวม** → **สำรองข้อมูลตอนนี้**

ถ้า token ถูกเพิกถอน (`invalid_grant`) หน้าเว็บจะแจ้งให้เชื่อมต่อใหม่

ปุ่ม **ยกเลิกการเชื่อมต่อ** ลบ `token.json` ในเครื่องเท่านั้น ไม่ได้เรียก revoke ที่ Google
เพราะการ revoke มีผลกับทั้ง grant ของ OAuth client นั้น ซึ่งอาจมีโปรแกรมอื่นใช้ client เดียวกันอยู่

## การใช้งาน

- **ภาพรวม** — สถานะ Google, รอบถัดไป, ความคืบหน้า (ไฟล์ที่ตรวจ/อัปโหลด/ย้ายถังขยะ/ขนาด/ไฟล์ปัจจุบัน),
  ประวัติแต่ละรอบ (เลื่อนลงในกล่องประวัติเพื่อโหลดรอบที่เก่ากว่า), ปุ่มสำรองตอนนี้/ยกเลิก/สแกนใหม่ทั้งหมด
- **โฟลเดอร์** — เพิ่ม (เลือกจากรายการโฟลเดอร์ในเครื่อง เริ่มที่ `~`) / นำออก, ดูจำนวนไฟล์และขนาด,
  แก้รูปแบบที่ไม่สำรอง (exclude)
- **ตั้งค่า** — ตารางเวลา, จำนวนไฟล์ที่อัปโหลดพร้อมกันและจำกัดความเร็วอัปโหลด, ID โฟลเดอร์หลักบน Drive,
  เชื่อมต่อ/ยกเลิกบัญชี Google
- **บันทึก** — log ของบริการ และรายการข้อผิดพลาดของแต่ละรอบ

รูปแบบ exclude (glob): `node_modules/` (ลงท้าย `/` = เฉพาะโฟลเดอร์), `*.tmp` (เทียบชื่อทุกระดับ),
`build/*.o` (มี `/` = เทียบ path จากโฟลเดอร์ต้นทาง) — ค่าเริ่มต้น
`node_modules/ .venv/ venv/ __pycache__/ .cache/ *.tmp *.swp .Trash-*/ vendor/ .git/ .next/ dist/ build/ .terraform/`
(บรรทัดที่ขึ้นต้นด้วย `#` เป็นหมายเหตุ ไม่ถูกใช้)

การอัปโหลด: แต่ละไฟล์ต้องรอ Google ตอบราว 1–2 วินาที จึงอัปโหลดหลายไฟล์พร้อมกัน (ค่าเริ่มต้น 4, ตั้งได้ 1–8)
ไฟล์เล็กจำนวนมากจะเสร็จเร็วขึ้นหลายเท่า ถ้าเน็ตช้าหรือต้องการเหลือแบนด์วิดท์ไว้ใช้งานอื่น ตั้ง
**จำกัดความเร็วอัปโหลด** (KB/s รวมทุกไฟล์, 0 = ไม่จำกัด) ซึ่งมีผลทันทีแม้กำลังสำรองอยู่

ข้อควรรู้เพราะเป็น mirror:
- ไฟล์ที่เคยสำรองแล้ว และภายหลังตรงกับ exclude ใหม่ จะถูกย้ายไปถังขยะบน Drive
- **สแกนใหม่ทั้งหมด** สร้างดัชนีใหม่จากของที่อยู่บน Drive แล้วซิงก์ ไฟล์บน Drive ใต้โฟลเดอร์ที่ mirror
  ที่ไม่มีในเครื่องจะถูกย้ายไปถังขยะ (ไฟล์ Google Docs/Sheets ไม่ถูกแตะ)
- เปลี่ยน ID โฟลเดอร์หลัก → รอบถัดไปเริ่มดัชนีใหม่สำหรับโฟลเดอร์นั้น (โฟลเดอร์เดิมบน Drive ไม่ถูกแตะ)

## พัฒนา

```bash
make test    # gofmt, go vet, go test (ใช้ Drive ปลอมในหน่วยความจำ ไม่ต่อเน็ต)
make build   # bin/laptop-backup
```

ทดลองรันโดยไม่แตะค่าจริง:

```bash
T=$(mktemp -d); HOME=$T XDG_CONFIG_HOME=$T/.config XDG_DATA_HOME=$T/.local/share bin/laptop-backup
```

ตัวเลือกของ binary: `-addr` (ค่าเริ่มต้น `127.0.0.1:8095`, ต้องเป็น loopback), `-host` ชื่อโฮสต์ของ proxy
ที่ยอมรับใน header `Host` (ค่าเริ่มต้นจาก `$LAPTOP_BACKUP_HOST` หรือ `backup`; ว่าง = รับเฉพาะ 127.0.0.1/localhost),
`-init` (เขียน config เริ่มต้นแล้วออก)

โครงสร้าง: `cmd/laptop-backup` (main), `internal/config`, `internal/auth` (OAuth),
`internal/drive` (+ `drivetest` ตัวปลอม), `internal/index` (SQLite), `internal/sync` (ตรรกะ mirror),
`internal/schedule`, `internal/web` (หน้าเว็บ + API, template/JS/CSS ฝังใน binary), `deploy/`

## ถอนการติดตั้ง

```bash
systemctl --user disable --now laptop-backup.service
rm -r ~/.config/systemd/user/laptop-backup.service ~/.config/systemd/user/laptop-backup.service.d
systemctl --user daemon-reload
rm ~/.local/bin/laptop-backup
# ถ้าตั้ง proxy ไว้ (แทน <PROXY_CONF_DIR>, <PROXY_CONTAINER>, <PROXY_HOST> ด้วยค่าที่ใช้):
rm <PROXY_CONF_DIR>/backup.conf
docker exec <PROXY_CONTAINER> nginx -s reload
sudo sed -i '/^127\.0\.0\.1[[:space:]]\+<PROXY_HOST>$/d' /etc/hosts
# ถ้าต้องการลบการตั้งค่า token และประวัติด้วย:
rm -rf ~/.config/laptop-backup ~/.local/share/laptop-backup
```

ไฟล์ที่สำรองไว้บน Google Drive ไม่ถูกลบ ถ้าต้องการเพิกถอนสิทธิ์ทั้งหมดไปที่
<https://myaccount.google.com/permissions> (ระวัง: มีผลกับโปรแกรมอื่นที่ใช้ OAuth client เดียวกันด้วย)
