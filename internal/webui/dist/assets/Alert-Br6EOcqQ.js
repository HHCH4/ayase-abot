import{D as d,G as t,E as b,c0 as W,C as N,d as O,K as k,h as o,f as l,c1 as D,V as K,Z,_ as G,p as x,r as X,c as h,M as i,a8 as Y,N as f,aO as q,a as J,L as Q,a6 as _,c2 as U,ak as ee,a2 as a,aa as re,aW as oe,aX as te,aZ as ne,aY as ae}from"./index-D1VezX-1.js";var se=d("alert",`
 line-height: var(--n-line-height);
 border-radius: var(--n-border-radius);
 position: relative;
 transition: background-color .3s var(--n-bezier);
 background-color: var(--n-color);
 text-align: start;
 word-break: break-word;
`,[t("border",`
 border-radius: inherit;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 transition: border-color .3s var(--n-bezier);
 border: var(--n-border);
 pointer-events: none;
 `),b("closable",[d("alert-body",[t("title",`
 padding-right: 24px;
 `)])]),t("icon",{color:"var(--n-icon-color)"}),d("alert-body",{padding:"var(--n-padding)"},[t("title",{color:"var(--n-title-text-color)"}),t("content",{color:"var(--n-content-text-color)"})]),W({originalTransition:"transform .3s var(--n-bezier)",enterToProps:{transform:"scale(1)"},leaveToProps:{transform:"scale(0.9)"}}),t("icon",`
 position: absolute;
 left: 0;
 top: 0;
 align-items: center;
 justify-content: center;
 display: flex;
 width: var(--n-icon-size);
 height: var(--n-icon-size);
 font-size: var(--n-icon-size);
 margin: var(--n-icon-margin);
 `),t("close",`
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier);
 position: absolute;
 right: 0;
 top: 0;
 margin: var(--n-close-margin);
 `),b("show-icon",[d("alert-body",{paddingLeft:"calc(var(--n-icon-margin-left) + var(--n-icon-size) + var(--n-icon-margin-right))"})]),b("right-adjust",[d("alert-body",{paddingRight:"calc(var(--n-close-size) + var(--n-padding) + 2px)"})]),d("alert-body",`
 border-radius: var(--n-border-radius);
 transition: border-color .3s var(--n-bezier);
 `,[t("title",`
 transition: color .3s var(--n-bezier);
 font-size: 16px;
 line-height: 19px;
 font-weight: var(--n-title-font-weight);
 `,[N("& +",[t("content",{marginTop:"9px"})])]),t("content",{transition:"color .3s var(--n-bezier)",fontSize:"var(--n-font-size)"})]),t("icon",{transition:"color .3s var(--n-bezier)"})]);const ie={...k.props,title:String,showIcon:{type:Boolean,default:!0},type:{type:String,default:"default"},bordered:{type:Boolean,default:!0},closable:Boolean,onClose:Function,onAfterLeave:Function,onAfterHide:Function};var fe=O({name:"Alert",inheritAttrs:!1,props:ie,slots:Object,setup(e){const{mergedClsPrefixRef:s,mergedBorderedRef:v,inlineThemeDisabled:u,mergedRtlRef:g}=K(e),m=k("Alert","-alert",se,U,e,s),R=Z("Alert",g,s),p=x(()=>{const{common:{cubicBezierEaseInOut:c},self:r}=m.value,{fontSize:$,borderRadius:P,titleFontWeight:w,lineHeight:B,iconSize:I,iconMargin:y,iconMarginRtl:T,closeIconSize:L,closeBorderRadius:E,closeSize:S,closeMargin:H,closeMarginRtl:M,padding:V}=r,{type:n}=e,{left:F,right:j}=ee(y);return{"--n-bezier":c,"--n-color":r[a("color",n)],"--n-close-icon-size":L,"--n-close-border-radius":E,"--n-close-color-hover":r[a("closeColorHover",n)],"--n-close-color-pressed":r[a("closeColorPressed",n)],"--n-close-icon-color":r[a("closeIconColor",n)],"--n-close-icon-color-hover":r[a("closeIconColorHover",n)],"--n-close-icon-color-pressed":r[a("closeIconColorPressed",n)],"--n-icon-color":r[a("iconColor",n)],"--n-border":r[a("border",n)],"--n-title-text-color":r[a("titleTextColor",n)],"--n-content-text-color":r[a("contentTextColor",n)],"--n-line-height":B,"--n-border-radius":P,"--n-font-size":$,"--n-title-font-weight":w,"--n-icon-size":I,"--n-icon-margin":y,"--n-icon-margin-rtl":T,"--n-close-size":S,"--n-close-margin":H,"--n-close-margin-rtl":M,"--n-padding":V,"--n-icon-margin-left":F,"--n-icon-margin-right":j}}),C=u?G("alert",x(()=>e.type[0]),p,e):void 0,z=X(!0),A=()=>{const{onAfterLeave:c,onAfterHide:r}=e;c&&c(),r&&r()};return{rtlEnabled:R,mergedClsPrefix:s,mergedBordered:v,visible:z,handleCloseClick:()=>{Promise.resolve(e.onClose?.()).then(c=>{c!==!1&&(z.value=!1)})},handleAfterLeave:()=>{A()},mergedTheme:m,cssVars:u?void 0:p,themeClass:C?.themeClass,onRender:C?.onRender}},render(){return this.onRender?.(),o(),l(D,{onAfterLeave:this.handleAfterLeave},{default:()=>{const{mergedClsPrefix:e,$slots:s}=this,v={class:[`${e}-alert`,this.themeClass,this.closable&&`${e}-alert--closable`,this.showIcon&&`${e}-alert--show-icon`,!this.title&&this.closable&&`${e}-alert--right-adjust`,this.rtlEnabled&&`${e}-alert--rtl`],style:this.cssVars,role:"alert"};return this.visible?(o(),h("div",_({key:1},_(this.$attrs,v)),[i(()=>this.closable&&(o(),l(Y,{clsPrefix:e,class:f(`${e}-alert__close`),onClick:this.handleCloseClick},null,8,["clsPrefix","class","onClick"]))),i(()=>this.bordered&&(o(),h("div",{class:f(`${e}-alert__border`)},null,2))),i(()=>this.showIcon&&(o(),h("div",{class:f(`${e}-alert__icon`),"aria-hidden":"true"},[i(()=>q(s.icon,()=>[(o(),l(re,{clsPrefix:e},{default:()=>{switch(this.type){case"success":return o(),l(ae,{key:3});case"info":return o(),l(ne,{key:4});case"warning":return o(),l(te,{key:5});case"error":return o(),l(oe,{key:6});default:return null}}},1032,["clsPrefix"]))]))],2))),J("div",{class:f([`${e}-alert-body`,this.mergedBordered&&`${e}-alert-body--bordered`])},[i(()=>Q(s.header,u=>{const g=u||this.title;return g?(o(),h("div",{key:2,class:f(`${e}-alert-body__title`)},[i(()=>g)],2)):null})),i(()=>s.default&&(o(),h("div",{class:f(`${e}-alert-body__content`)},[i(()=>s.default())],2)))],2)],16)):null}},1032,["onAfterLeave"])}});export{fe as A};
