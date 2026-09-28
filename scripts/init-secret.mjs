import { randomBytes } from 'node:crypto';
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const destination = new URL('../.dev.vars', import.meta.url);
try {
  writeFileSync(destination, `PROXY_ADMIN_KEY=${randomBytes(32).toString('hex')}\n`, {
    flag: 'wx',
    mode: 0o600,
  });
  console.log(`管理员密钥已随机生成，保存在 ${fileURLToPath(destination)}。`);
  console.log('打开该文件查看并备份密钥；部署表单填写等号右侧的值。');
} catch (error) {
  if (error.code === 'EEXIST') {
    console.error('.dev.vars 已存在，未覆盖。请使用现有密钥，避免已有账号密文无法解密。');
  } else {
    console.error(`无法保存管理员密钥：${error.code ?? 'unknown error'}`);
  }
  process.exitCode = 1;
}
