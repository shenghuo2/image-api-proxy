import { useEffect, useRef, useState, type InputHTMLAttributes } from 'react'

type Props = Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'value' | 'onChange'> & {
  value: number
  onChange: (value: number) => void
}

// Keep the text being edited separate from the last valid numeric value.
// Empty text and a lone minus sign must survive until the user finishes typing.
export function NumberInput({ value, onChange, min, max, step = 1, required = true, ...props }: Props) {
  const [draft, setDraft] = useState(String(value))
  const lastValue = useRef(value)
  const input = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!Object.is(lastValue.current, value)) setDraft(String(value))
    lastValue.current = value
  }, [value])

  useEffect(() => {
    const parsed = Number(draft)
    let error = ''
    if (!draft.trim()) error = required ? '请输入数字' : ''
    else if (!/^-?(?:\d+(?:\.\d*)?|\.\d+)$/.test(draft) || !Number.isFinite(parsed)) error = '请输入有效数字'
    else if (Number(step) === 1 && !Number.isInteger(parsed)) error = '请输入整数'
    else if (min !== undefined && parsed < Number(min)) error = `不能小于 ${min}`
    else if (max !== undefined && parsed > Number(max)) error = `不能大于 ${max}`
    input.current?.setCustomValidity(error)
  }, [draft, min, max, step, required])

  return <input {...props} ref={input} type="text" inputMode={min !== undefined && Number(min) >= 0 ? Number(step) === 1 ? 'numeric' : 'decimal' : 'text'} value={draft} required={required} onChange={(event) => {
    const text = event.target.value
    setDraft(text)
    if (/^-?(?:\d+(?:\.\d*)?|\.\d+)$/.test(text) && Number.isFinite(Number(text))) {
      lastValue.current = Number(text)
      onChange(lastValue.current)
    }
  }} onBlur={(event) => {
    if (event.currentTarget.validity.valid && draft.trim()) setDraft(String(Number(draft)))
    props.onBlur?.(event)
  }} />
}
