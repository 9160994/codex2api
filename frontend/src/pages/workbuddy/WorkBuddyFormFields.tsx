import { useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Eye, EyeOff } from "lucide-react";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// WorkBuddy 弹窗共用的小字段件：标题、说明、多行输入、密钥输入。
// 与 Antigravity 的表单件同构（DESIGN.md §1 要求表单控件只用共享 ui 组件）。

export const TEXTAREA_CLASS =
  "w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30";

export function FieldLabel({ children }: { children: ReactNode }) {
  return <span className="text-xs font-semibold text-muted-foreground">{children}</span>;
}

export function FieldHint({ children, tone = "muted" }: { children: ReactNode; tone?: "muted" | "warning" }) {
  return (
    <p
      className={cn(
        "text-[11px] leading-relaxed",
        tone === "warning" ? "text-amber-700 dark:text-amber-300" : "text-muted-foreground",
      )}
    >
      {children}
    </p>
  );
}

/** 密钥输入：默认遮盖，可切换明文核对。 */
export function SecretInput({
  value,
  onChange,
  placeholder,
  autoComplete = "new-password",
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  autoComplete?: string;
}) {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  return (
    <div className="relative">
      <Input
        type={visible ? "text" : "password"}
        autoComplete={autoComplete}
        spellCheck={false}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        className="pr-10 font-mono"
      />
      <button
        type="button"
        onClick={() => setVisible((current) => !current)}
        className="absolute right-1.5 top-1/2 flex size-7 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground"
        aria-label={visible ? t("workbuddy.hideSecret") : t("workbuddy.showSecret")}
        title={visible ? t("workbuddy.hideSecret") : t("workbuddy.showSecret")}
      >
        {visible ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
      </button>
    </div>
  );
}

/** ISO 时间 → <input type="datetime-local"> 的本地值（空值原样返回空串）。 */
export function isoToDateTimeLocal(iso?: string | null): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}`;
}

/** datetime-local 的本地值 → ISO；无法解析时返回空串。 */
export function dateTimeLocalToISO(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return "";
  const date = new Date(trimmed);
  return Number.isNaN(date.getTime()) ? "" : date.toISOString();
}
