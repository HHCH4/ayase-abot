import{D as Y,G as a,H as X,C as U,E as l,ab as G,d as ve,K as q,aH as j,h as u,c as m,a as p,M as r,L as v,N as n,f as J,Q,V as ge,Y as we,r as E,W as me,_ as pe,p as P,a1 as A,aI as ye,a6 as ke,O as xe,aJ as Se,a2 as g,aK as I,aL as s,a3 as _e}from"./index-BLhoHfwf.js";var ze=Y("switch",`
 height: var(--n-height);
 min-width: var(--n-width);
 vertical-align: middle;
 user-select: none;
 -webkit-user-select: none;
 display: inline-flex;
 outline: none;
 justify-content: center;
 align-items: center;
`,[a("children-placeholder",`
 height: var(--n-rail-height);
 display: flex;
 flex-direction: column;
 overflow: hidden;
 pointer-events: none;
 visibility: hidden;
 `),a("rail-placeholder",`
 display: flex;
 flex-wrap: none;
 `),a("button-placeholder",`
 width: calc(1.75 * var(--n-rail-height));
 height: var(--n-rail-height);
 `),Y("base-loading",`
 position: absolute;
 top: 50%;
 left: 50%;
 transform: translateX(-50%) translateY(-50%);
 font-size: calc(var(--n-button-width) - 4px);
 color: var(--n-loading-color);
 transition: color .3s var(--n-bezier);
 `,[X({left:"50%",top:"50%",originalTransform:"translateX(-50%) translateY(-50%)"})]),a("checked, unchecked",`
 transition: color .3s var(--n-bezier);
 color: var(--n-text-color);
 box-sizing: border-box;
 position: absolute;
 white-space: nowrap;
 top: 0;
 bottom: 0;
 display: flex;
 align-items: center;
 line-height: 1;
 `),a("checked",`
 right: 0;
 padding-right: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),a("unchecked",`
 left: 0;
 justify-content: flex-end;
 padding-left: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),U("&:focus",[a("rail",`
 box-shadow: var(--n-box-shadow-focus);
 `)]),l("round",[a("rail","border-radius: calc(var(--n-rail-height) / 2);",[a("button","border-radius: calc(var(--n-button-height) / 2);")])]),G("disabled",[G("icon",[l("rubber-band",[l("pressed",[a("rail",[a("button","max-width: var(--n-button-width-pressed);")])]),a("rail",[U("&:active",[a("button","max-width: var(--n-button-width-pressed);")])]),l("active",[l("pressed",[a("rail",[a("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])]),a("rail",[U("&:active",[a("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])])])])])]),l("active",[a("rail",[a("button","left: calc(100% - var(--n-button-width) - var(--n-offset))")])]),a("rail",`
 overflow: hidden;
 height: var(--n-rail-height);
 min-width: var(--n-rail-width);
 border-radius: var(--n-rail-border-radius);
 cursor: pointer;
 position: relative;
 transition:
 opacity .3s var(--n-bezier),
 background .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 background-color: var(--n-rail-color);
 `,[a("button-icon",`
 color: var(--n-icon-color);
 transition: color .3s var(--n-bezier);
 font-size: calc(var(--n-button-height) - 4px);
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 display: flex;
 justify-content: center;
 align-items: center;
 line-height: 1;
 `,[X()]),a("button",`
 align-items: center; 
 top: var(--n-offset);
 left: var(--n-offset);
 height: var(--n-button-height);
 width: var(--n-button-width-pressed);
 max-width: var(--n-button-width);
 border-radius: var(--n-button-border-radius);
 background-color: var(--n-button-color);
 box-shadow: var(--n-button-box-shadow);
 box-sizing: border-box;
 cursor: inherit;
 content: "";
 position: absolute;
 transition:
 background-color .3s var(--n-bezier),
 left .3s var(--n-bezier),
 opacity .3s var(--n-bezier),
 max-width .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 `)]),l("active",[a("rail","background-color: var(--n-rail-color-active);")]),l("loading",[a("rail",`
 cursor: wait;
 `)]),l("disabled",[a("rail",`
 cursor: not-allowed;
 opacity: .5;
 `)])]);const $e=["aria-checked","tabindex","onClick","onFocus","onBlur","onKeyup","onKeydown"],Ce={...q.props,size:String,value:{type:[String,Number,Boolean],default:void 0},loading:Boolean,defaultValue:{type:[String,Number,Boolean],default:!1},disabled:{type:Boolean,default:void 0},round:{type:Boolean,default:!0},"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],checkedValue:{type:[String,Number,Boolean],default:!0},uncheckedValue:{type:[String,Number,Boolean],default:!1},railStyle:Function,rubberBand:{type:Boolean,default:!0},spinProps:Object,onChange:[Function,Array]};let z;var Ve=ve({name:"Switch",props:Ce,slots:Object,setup(e){z===void 0&&(typeof CSS<"u"?typeof CSS.supports<"u"?z=CSS.supports("width","max(1px)"):z=!1:z=!0);const{mergedClsPrefixRef:$,inlineThemeDisabled:y,mergedComponentPropsRef:K}=ge(e),T=q("Switch","-switch",ze,Se,e,$),w=we(e,{mergedSize(t){if(e.size!==void 0)return e.size;if(t)return t.mergedSize.value;const b=K?.value?.Switch?.size;return b||"medium"}}),{mergedSizeRef:k,mergedDisabledRef:h}=w,x=E(e.defaultValue),C=_e(e,"value"),f=me(C,x),B=P(()=>f.value===e.checkedValue),i=E(!1),o=E(!1),S=P(()=>{const{railStyle:t}=e;if(t)return t({focused:o.value,checked:B.value})});function V(t){const{"onUpdate:value":b,onChange:R,onUpdateValue:F}=e,{nTriggerFormInput:W,nTriggerFormChange:D}=w;b&&A(b,t),F&&A(F,t),R&&A(R,t),x.value=t,W(),D()}function Z(){const{nTriggerFormFocus:t}=w;t()}function ee(){const{nTriggerFormBlur:t}=w;t()}function te(){e.loading||h.value||(f.value!==e.checkedValue?V(e.checkedValue):V(e.uncheckedValue))}function ae(){o.value=!0,Z()}function ie(){o.value=!1,ee(),i.value=!1}function ne(t){e.loading||h.value||t.key===" "&&(f.value!==e.checkedValue?V(e.checkedValue):V(e.uncheckedValue),i.value=!1)}function oe(t){e.loading||h.value||t.key===" "&&(t.preventDefault(),i.value=!0)}const L=P(()=>{const{value:t}=k,{self:{opacityDisabled:b,railColor:R,railColorActive:F,buttonBoxShadow:W,buttonColor:D,boxShadowFocus:re,loadingColor:le,textColor:se,iconColor:ce,[g("buttonHeight",t)]:c,[g("buttonWidth",t)]:de,[g("buttonWidthPressed",t)]:ue,[g("railHeight",t)]:d,[g("railWidth",t)]:_,[g("railBorderRadius",t)]:he,[g("buttonBorderRadius",t)]:fe},common:{cubicBezierEaseInOut:be}}=T.value;let H,N,M;return z?(H=`calc((${d} - ${c}) / 2)`,N=`max(${d}, ${c})`,M=`max(${_}, calc(${_} + ${c} - ${d}))`):(H=I((s(d)-s(c))/2),N=I(Math.max(s(d),s(c))),M=s(d)>s(c)?_:I(s(_)+s(c)-s(d))),{"--n-bezier":be,"--n-button-border-radius":fe,"--n-button-box-shadow":W,"--n-button-color":D,"--n-button-width":de,"--n-button-width-pressed":ue,"--n-button-height":c,"--n-height":N,"--n-offset":H,"--n-opacity-disabled":b,"--n-rail-border-radius":he,"--n-rail-color":R,"--n-rail-color-active":F,"--n-rail-height":d,"--n-rail-width":_,"--n-width":M,"--n-box-shadow-focus":re,"--n-loading-color":le,"--n-text-color":se,"--n-icon-color":ce}}),O=y?pe("switch",P(()=>k.value[0]),L,e):void 0;return{handleClick:te,handleBlur:ie,handleFocus:ae,handleKeyup:ne,handleKeydown:oe,mergedRailStyle:S,pressed:i,mergedClsPrefix:$,mergedValue:f,checked:B,mergedDisabled:h,cssVars:y?void 0:L,themeClass:O?.themeClass,onRender:O?.onRender}},render(){const{mergedClsPrefix:e,mergedDisabled:$,checked:y,mergedRailStyle:K,onRender:T,$slots:w}=this;T?.();const{checked:k,unchecked:h,icon:x,"checked-icon":C,"unchecked-icon":f}=w,B=!(j(x)&&j(C)&&j(f));return u(),m("div",{role:"switch","aria-checked":y,class:n([`${e}-switch`,this.themeClass,B&&`${e}-switch--icon`,y&&`${e}-switch--active`,$&&`${e}-switch--disabled`,this.round&&`${e}-switch--round`,this.loading&&`${e}-switch--loading`,this.pressed&&`${e}-switch--pressed`,this.rubberBand&&`${e}-switch--rubber-band`]),tabindex:this.mergedDisabled?void 0:0,style:Q(this.cssVars),onClick:this.handleClick,onFocus:this.handleFocus,onBlur:this.handleBlur,onKeyup:this.handleKeyup,onKeydown:this.handleKeydown},[p("div",{class:n(`${e}-switch__rail`),"aria-hidden":"true",style:Q(K)},[r(()=>v(k,i=>v(h,o=>i||o?(u(),m("div",{key:4,"aria-hidden":!0,class:n(`${e}-switch__children-placeholder`)},[p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>i)],2),p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>o)],2)],2)):null))),p("div",{class:n(`${e}-switch__button`)},[r(()=>v(x,i=>v(C,o=>v(f,S=>(u(),J(xe,null,{default:()=>this.loading?(u(),J(ye,ke({key:"loading",clsPrefix:e,strokeWidth:20},this.spinProps),null,16,["clsPrefix"])):this.checked&&(o||i)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:o?"checked-icon":"icon"},[r(()=>o||i)],2)):!this.checked&&(S||i)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:S?"unchecked-icon":"icon"},[r(()=>S||i)],2)):null},1024)))))),r(()=>v(k,i=>i&&(u(),m("div",{key:"checked",class:n(`${e}-switch__checked`)},[r(()=>i)],2)))),r(()=>v(h,i=>i&&(u(),m("div",{key:"unchecked",class:n(`${e}-switch__unchecked`)},[r(()=>i)],2))))],2)],6)],46,$e)}});export{Ve as S};
