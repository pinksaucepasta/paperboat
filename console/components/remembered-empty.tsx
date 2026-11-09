/**
 * A list whose empty state was remembered (useLastCount, `<name>:<workspace>:<machine>` = 1) while
 * the browser can't read it yet (the prerendered shell, the server's render): both forms are drawn,
 * and the inline script after them, run as the HTML is parsed, shows the empty state before
 * anything is painted. The machine comes from the URL (`?machine=` or a machine's page), which the
 * prerendered shell doesn't know.
 */
export function RememberedEmpty({
  name,
  empty,
  children,
}: {
  name: string
  empty: React.ReactNode
  children: React.ReactNode
}) {
  const script =
    `try{var w=(document.cookie.match(/(?:^|; )pb-workspace=([a-z0-9-]+)/)||[,"personal"])[1];` +
    `var m=new URLSearchParams(location.search).get("machine")||(location.pathname.match(/^\\/machines\\/([^/]+)/)||[])[1]||"";` +
    `if(localStorage.getItem(${JSON.stringify(`pb-rows:${name}:`)}+w+":"+m)==="1"){` +
    `var l=document.currentScript.previousElementSibling;l.hidden=true;l.previousElementSibling.hidden=false}}catch(e){}`
  return (
    <>
      <div hidden suppressHydrationWarning>
        {empty}
      </div>
      <div suppressHydrationWarning>{children}</div>
      <script dangerouslySetInnerHTML={{ __html: script }} />
    </>
  )
}
