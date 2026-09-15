import{C as e,D as n,aU as ae,E as i,G as a,I as de,J as le,d as se,K as E,h as g,f as $,w as ie,M as l,Q as m,N as c,V as ce,Z as be,_ as ge,a1 as fe,p as w,L as f,b7 as v,c as h,a as ve,a8 as he,b2 as me,b8 as pe,a2 as B,ak as ue}from"./index-D1VezX-1.js";import{k as Ce}from"./keysOf-HiGXOwLp.js";const _=n("card-content",`
 flex: 1;
 min-width: 0;
 box-sizing: border-box;
 padding: 0 var(--n-padding-left) var(--n-padding-bottom) var(--n-padding-left);
 font-size: var(--n-font-size);
`);var xe=e([n("card",`
 font-size: var(--n-font-size);
 line-height: var(--n-line-height);
 display: flex;
 flex-direction: column;
 width: 100%;
 box-sizing: border-box;
 position: relative;
 border-radius: var(--n-border-radius);
 background-color: var(--n-color);
 color: var(--n-text-color);
 word-break: break-word;
 transition: 
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
 `,[ae({background:"var(--n-color-modal)"}),i("hoverable",[e("&:hover","box-shadow: var(--n-box-shadow);")]),i("content-segmented",[e(">",[n("card-content",`
 padding-top: var(--n-padding-bottom);
 `),a("content-scrollbar",[e(">",[n("scrollbar-container",[e(">",[n("card-content",`
 padding-top: var(--n-padding-bottom);
 `)])])])])])]),i("content-soft-segmented",[e(">",[n("card-content",`
 margin: 0 var(--n-padding-left);
 padding: var(--n-padding-bottom) 0;
 `),a("content-scrollbar",[e(">",[n("scrollbar-container",[e(">",[n("card-content",`
 margin: 0 var(--n-padding-left);
 padding: var(--n-padding-bottom) 0;
 `)])])])])])]),i("footer-segmented",[e(">",[a("footer",`
 padding-top: var(--n-padding-bottom);
 `)])]),i("footer-soft-segmented",[e(">",[a("footer",`
 padding: var(--n-padding-bottom) 0;
 margin: 0 var(--n-padding-left);
 `)])]),e(">",[n("card-header",`
 box-sizing: border-box;
 display: flex;
 align-items: center;
 font-size: var(--n-title-font-size);
 padding:
 var(--n-padding-top)
 var(--n-padding-left)
 var(--n-padding-bottom)
 var(--n-padding-left);
 `,[a("main",`
 font-weight: var(--n-title-font-weight);
 transition: color .3s var(--n-bezier);
 flex: 1;
 min-width: 0;
 color: var(--n-title-text-color);
 `),a("extra",`
 display: flex;
 align-items: center;
 font-size: var(--n-font-size);
 font-weight: 400;
 transition: color .3s var(--n-bezier);
 color: var(--n-text-color);
 `),a("close",`
 margin: 0 0 0 8px;
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `)]),a("action",`
 box-sizing: border-box;
 transition:
 background-color .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
 background-clip: padding-box;
 background-color: var(--n-action-color);
 `),_,n("card-content",[e("&:first-child",`
 padding-top: var(--n-padding-bottom);
 `)]),a("content-scrollbar",`
 display: flex;
 flex-direction: column;
 `,[e(">",[n("scrollbar-container",[e(">",[_])])]),e("&:first-child >",[n("scrollbar-container",[e(">",[n("card-content",`
 padding-top: var(--n-padding-bottom);
 `)])])])]),a("footer",`
 box-sizing: border-box;
 padding: 0 var(--n-padding-left) var(--n-padding-bottom) var(--n-padding-left);
 font-size: var(--n-font-size);
 `,[e("&:first-child",`
 padding-top: var(--n-padding-bottom);
 `)]),a("action",`
 background-color: var(--n-action-color);
 padding: var(--n-padding-bottom) var(--n-padding-left);
 border-bottom-left-radius: var(--n-border-radius);
 border-bottom-right-radius: var(--n-border-radius);
 `)]),n("card-cover",`
 overflow: hidden;
 width: 100%;
 border-radius: var(--n-border-radius) var(--n-border-radius) 0 0;
 `,[e("img",`
 display: block;
 width: 100%;
 `)]),i("bordered",`
 border: 1px solid var(--n-border-color);
 `,[e("&:target","border-color: var(--n-color-target);")]),i("action-segmented",[e(">",[a("action",[e("&:not(:first-child)",`
 border-top: 1px solid var(--n-border-color);
 `)])])]),i("content-segmented, content-soft-segmented",[e(">",[n("card-content",`
 transition: border-color 0.3s var(--n-bezier);
 `,[e("&:not(:first-child)",`
 border-top: 1px solid var(--n-border-color);
 `)]),a("content-scrollbar",`
 transition: border-color 0.3s var(--n-bezier);
 `,[e("&:not(:first-child)",`
 border-top: 1px solid var(--n-border-color);
 `)])])]),i("footer-segmented, footer-soft-segmented",[e(">",[a("footer",`
 transition: border-color 0.3s var(--n-bezier);
 `,[e("&:not(:first-child)",`
 border-top: 1px solid var(--n-border-color);
 `)])])]),i("embedded",`
 background-color: var(--n-color-embedded);
 `)]),de(n("card",`
 background: var(--n-color-modal);
 `,[i("embedded",`
 background-color: var(--n-color-embedded-modal);
 `)])),le(n("card",`
 background: var(--n-color-popover);
 `,[i("embedded",`
 background-color: var(--n-color-embedded-popover);
 `)]))]);const P={title:[String,Function],contentClass:String,contentStyle:[Object,String],contentScrollable:Boolean,headerClass:String,headerStyle:[Object,String],headerExtraClass:String,headerExtraStyle:[Object,String],footerClass:String,footerStyle:[Object,String],embedded:Boolean,segmented:{type:[Boolean,Object],default:!1},size:String,bordered:{type:Boolean,default:!0},closable:Boolean,hoverable:Boolean,role:String,onClose:[Function,Array],tag:{type:String,default:"div"},cover:Function,content:[String,Function],footer:Function,action:Function,headerExtra:Function,closeFocusable:Boolean},ke=Ce(P),ze={...E.props,...P};var $e=se({name:"Card",props:ze,slots:Object,setup(t){const x=()=>{const{onClose:d}=t;d&&fe(d)},{inlineThemeDisabled:p,mergedClsPrefixRef:o,mergedRtlRef:z,mergedComponentPropsRef:S}=ce(t),u=E("Card","-card",xe,pe,t,o),y=be("Card",z,o),b=w(()=>t.size||S?.value?.Card?.size||"medium"),s=w(()=>{const d=b.value,{self:{color:k,colorModal:C,colorTarget:R,textColor:F,titleTextColor:O,titleFontWeight:V,borderColor:M,actionColor:T,borderRadius:j,lineHeight:I,closeIconColor:H,closeIconColorHover:N,closeIconColorPressed:K,closeColorHover:L,closeColorPressed:D,closeBorderRadius:W,closeIconSize:A,closeSize:G,boxShadow:J,colorPopover:Q,colorEmbedded:U,colorEmbeddedModal:Z,colorEmbeddedPopover:q,[B("padding",d)]:X,[B("fontSize",d)]:Y,[B("titleFontSize",d)]:ee},common:{cubicBezierEaseInOut:oe}}=u.value,{top:re,left:te,bottom:ne}=ue(X);return{"--n-bezier":oe,"--n-border-radius":j,"--n-color":k,"--n-color-modal":C,"--n-color-popover":Q,"--n-color-embedded":U,"--n-color-embedded-modal":Z,"--n-color-embedded-popover":q,"--n-color-target":R,"--n-text-color":F,"--n-line-height":I,"--n-action-color":T,"--n-title-text-color":O,"--n-title-font-weight":V,"--n-close-icon-color":H,"--n-close-icon-color-hover":N,"--n-close-icon-color-pressed":K,"--n-close-color-hover":L,"--n-close-color-pressed":D,"--n-border-color":M,"--n-box-shadow":J,"--n-padding-top":re,"--n-padding-bottom":ne,"--n-padding-left":te,"--n-font-size":Y,"--n-title-font-size":ee,"--n-close-size":G,"--n-close-icon-size":A,"--n-close-border-radius":W}}),r=p?ge("card",w(()=>b.value[0]),s,t):void 0;return{rtlEnabled:y,mergedClsPrefix:o,mergedTheme:u,handleCloseClick:x,cssVars:p?void 0:s,themeClass:r?.themeClass,onRender:r?.onRender}},render(){const{segmented:t,bordered:x,hoverable:p,mergedClsPrefix:o,rtlEnabled:z,onRender:S,embedded:u,tag:y,$slots:b}=this;return S?.(),g(),$(y,{class:c([`${o}-card`,this.themeClass,u&&`${o}-card--embedded`,{[`${o}-card--rtl`]:z,[`${o}-card--content-scrollable`]:this.contentScrollable,[`${o}-card--content${typeof t!="boolean"&&t.content==="soft"?"-soft":""}-segmented`]:t===!0||t!==!1&&t.content,[`${o}-card--footer${typeof t!="boolean"&&t.footer==="soft"?"-soft":""}-segmented`]:t===!0||t!==!1&&t.footer,[`${o}-card--action-segmented`]:t===!0||t!==!1&&t.action,[`${o}-card--bordered`]:x,[`${o}-card--hoverable`]:p}]),style:m(this.cssVars),role:this.role},{default:ie(()=>[l(()=>f(b.cover,s=>{const r=this.cover?v([this.cover()]):s;return r&&(g(),h("div",{class:c(`${o}-card-cover`),role:"none"},[l(()=>r)],2))})),l(()=>f(b.header,s=>{const{title:r}=this,d=r?v(typeof r=="function"?[r()]:[r]):s;return d||this.closable?(g(),h("div",{key:1,class:c([`${o}-card-header`,this.headerClass]),style:m(this.headerStyle),role:"heading"},[ve("div",{class:c(`${o}-card-header__main`),role:"heading"},[l(()=>d)],2),l(()=>f(b["header-extra"],k=>{const C=this.headerExtra?v([this.headerExtra()]):k;return C&&(g(),h("div",{class:c([`${o}-card-header__extra`,this.headerExtraClass]),style:m(this.headerExtraStyle)},[l(()=>C)],6))})),l(()=>this.closable&&(g(),$(he,{clsPrefix:o,class:c(`${o}-card-header__close`),onClick:this.handleCloseClick,focusable:this.closeFocusable,absolute:!0},null,8,["clsPrefix","class","onClick","focusable"])))],6)):null})),l(()=>f(b.default,s=>{const{content:r}=this,d=r?v(typeof r=="function"?[r()]:[r]):s;return d?this.contentScrollable?(g(),$(me,{key:2,class:c(`${o}-card__content-scrollbar`),contentClass:[`${o}-card-content`,this.contentClass],contentStyle:this.contentStyle},{default:()=>d},1032,["class","contentClass","contentStyle"])):(g(),h("div",{key:3,class:c([`${o}-card-content`,this.contentClass]),style:m(this.contentStyle),role:"none"},[l(()=>d)],6)):null})),l(()=>f(b.footer,s=>{const r=this.footer?v([this.footer()]):s;return r&&(g(),h("div",{class:c([`${o}-card__footer`,this.footerClass]),style:m(this.footerStyle),role:"none"},[l(()=>r)],6))})),l(()=>f(b.action,s=>{const r=this.action?v([this.action()]):s;return r&&(g(),h("div",{class:c(`${o}-card__action`),role:"none"},[l(()=>r)],2))}))]),_:2},1032,["class","style","role"])}});export{$e as C,ke as a,P as c};
